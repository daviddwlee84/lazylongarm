package flows

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/cloudfw"
	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/hostprobe"
	"github.com/daviddwlee84/lazylongarm/internal/hubhost"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/secret"
	"github.com/daviddwlee84/lazylongarm/internal/tailscalehost"
)

// PanelAccessRequest is `hub panel-access`: how the web panel is reached
// beyond the SSH tunnel.
type PanelAccessRequest struct {
	Hub       *config.Hub // the hub we operate (SSHHost required)
	Mode      string      // config.PanelLoopback | PanelTailnet | PanelPublic
	Domain    string      // public: certificate name (default: the hub address's host)
	ACMEEmail string      // public: optional Let's Encrypt contact
	Redirect  bool        // public: also serve http :80 -> https
	AuthKey   secret.Secret
	// HubConfig is the current /etc/longarm-hub/hub.toml (read over SSH by
	// the caller); public mode and the revert rewrite it from these values.
	HubConfig string
}

// PanelAccessDeps are side effects, injectable for tests.
type PanelAccessDeps struct {
	Config *config.Config
	// Rules pre-planned per NSG for the review (public mode).
	Rules map[string][]cloudfw.Planned
	// Get fetches url and fails on a non-2xx status or a bad certificate.
	Get func(ctx context.Context, url string) error
}

// DefaultPanelDomain is the host part of the hub address.
func DefaultPanelDomain(h *config.Hub) string {
	host, _, err := net.SplitHostPort(h.Address)
	if err != nil {
		return h.Address
	}
	return host
}

// PanelAccessDefaults fills unset fields.
func PanelAccessDefaults(req PanelAccessRequest) PanelAccessRequest {
	if req.Mode == config.PanelPublic && req.Domain == "" && req.Hub != nil {
		req.Domain = DefaultPanelDomain(req.Hub)
	}
	req.Domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(req.Domain)), ".")
	return req
}

// ValidatePanelAccess checks a request against the hub and its probed host.
func ValidatePanelAccess(req PanelAccessRequest, f *hostprobe.Facts) error {
	var problems []string
	h := req.Hub
	if h == nil || h.SSHHost == "" {
		return errors.New("panel access needs a hub lazylongarm operates over SSH (hub setup records it)")
	}
	switch req.Mode {
	case config.PanelLoopback, config.PanelTailnet, config.PanelPublic:
	default:
		problems = append(problems, "mode must be loopback, tailnet or public")
	}
	if req.Mode == config.PanelPublic {
		if h.Mode != config.ModePublic {
			problems = append(problems, "public panel access needs a public hub (this one is "+h.Mode+")")
		}
		switch {
		case req.Domain == "":
			problems = append(problems, "a DNS name for the panel certificate is required")
		case net.ParseIP(req.Domain) != nil:
			problems = append(problems, "Let's Encrypt needs a DNS name, not an IP address")
		case strings.ContainsAny(req.Domain, " /:'\"@"):
			problems = append(problems, "domain must be a bare DNS name (no scheme, port or path)")
		case !strings.Contains(req.Domain, "."):
			problems = append(problems, "domain must be a fully qualified DNS name")
		}
		if req.ACMEEmail != "" && !strings.Contains(req.ACMEEmail, "@") {
			problems = append(problems, "acme e-mail must be an e-mail address")
		}
		if f != nil && h.PanelMode() != config.PanelPublic {
			ports := []string{"tcp/443"}
			if req.Redirect {
				ports = append(ports, "tcp/80")
			}
			for _, p := range ports {
				if addr, busy := f.Ports()[p]; busy {
					problems = append(problems, fmt.Sprintf("%s is already in use on %s (%s); free it, or use tailnet mode", p, f.Where(), addr))
				}
			}
		}
	}
	if (req.Mode == config.PanelPublic || h.PanelMode() == config.PanelPublic) && req.HubConfig == "" {
		problems = append(problems, "the current hub.toml could not be read")
	}
	if req.Mode == config.PanelTailnet && f != nil && !f.Has["tailscale"] {
		if f.OS != "linux" || (f.DistroID != "ubuntu" && f.DistroID != "debian") || f.DistroCode == "" {
			problems = append(problems, fmt.Sprintf("installing Tailscale is supported on Debian/Ubuntu (apt) for now; %s is %s — install it yourself and rerun", f.Where(), f.Distro))
		}
		for _, c := range []string{"curl", "gpg", "systemctl"} {
			if !f.Has[c] {
				problems = append(problems, c+" is required on the hub host to install Tailscale")
			}
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// BuildPanelAccess assembles the plan that moves the panel to req.Mode,
// undoing the previous mode's changes on the way.
func BuildPanelAccess(req PanelAccessRequest, f *hostprobe.Facts, deps PanelAccessDeps) (*plan.Plan, error) {
	req = PanelAccessDefaults(req)
	if err := ValidatePanelAccess(req, f); err != nil {
		return nil, err
	}
	h := req.Hub
	host := h.SSHHost
	panel := orDefault(h.Panel, "127.0.0.1:7480")
	prev := h.PanelMode()
	p := &plan.Plan{
		Kind:  "hub-panel-access",
		Key:   h.ID,
		Title: fmt.Sprintf("Panel access for hub %s: %s", h.ID, req.Mode),
		Summary: [][2]string{
			{"hub", fmt.Sprintf("%s (ssh %s)", h.Address, host)},
			{"now", describePanel(prev, h.PanelURL())},
		},
	}

	var current hubhost.Settings
	handEdited := false
	if req.HubConfig != "" {
		s, edited, err := hubhost.ParseSettings(req.HubConfig)
		if err != nil {
			return nil, err
		}
		current, handEdited = s, edited
	}

	// Undo what the other modes changed.
	if req.Mode != config.PanelTailnet && (req.Mode == config.PanelLoopback || prev == config.PanelTailnet) {
		p.Steps = append(p.Steps, tailscaleServeOffStep(host))
	}
	if req.Mode != config.PanelPublic && (req.Mode == config.PanelLoopback || prev == config.PanelPublic) {
		if current.HTTPS != "" {
			off := current
			off.PublicURL, off.HTTPS, off.HTTPSRedirect, off.ACMEEmail = "", "", "", ""
			p.Steps = append(p.Steps, hubConfigStep(host, off))
		}
		p.Steps = append(p.Steps, removeDropInStep(host), restartHubStep(host, panel))
		if nsg := panelNSG(h); nsg != "" && (prev == config.PanelPublic || hasPanelRules(h)) {
			p.Steps = append(p.Steps, closePanelRulesStep(nsg, cloudfw.PanelRules(true)))
		}
	}

	var url string
	switch req.Mode {
	case config.PanelLoopback:
		p.Summary = append(p.Summary, [2]string{"after", describePanel(config.PanelLoopback, "")})
		if f != nil && f.Has["tailscale"] {
			p.Warnings = append(p.Warnings, "Tailscale stays installed and logged in on "+host+"; `sudo tailscale logout` (or remove the machine in the admin console) if you no longer need it")
		}
	case config.PanelTailnet:
		p.Summary = append(p.Summary,
			[2]string{"after", "https://<hub>.<tailnet>.ts.net/ via tailscale serve: only devices on your tailnet can open it; panel login (password + TOTP) still applies"},
			[2]string{"tailscale", tailscaleSummary(f)})
		if req.AuthKey.Empty() {
			p.Warnings = append(p.Warnings, "if Tailscale is not logged in yet, the login step hands the terminal to `tailscale up`: open the URL it prints (on any device) and approve the machine")
		}
		p.Warnings = append(p.Warnings, "tailscale serve needs MagicDNS and HTTPS Certificates switched on for your tailnet ("+tailscalehost.AdminDNS+")")
		p.Steps = append(p.Steps, tailnetSteps(req, f, host, panel)...)
	case config.PanelPublic:
		target := current
		target.PublicURL, target.HTTPS, target.ACMEEmail = "https://"+req.Domain, ":443", req.ACMEEmail
		target.HTTPSRedirect = ""
		if req.Redirect {
			target.HTTPSRedirect = ":80"
		}
		url = "https://" + req.Domain + "/"
		ports := "443/tcp"
		if req.Redirect {
			ports += " + 80/tcp (redirect)"
		}
		p.Summary = append(p.Summary,
			[2]string{"after", url + " on the Internet (longarm-hub's built-in HTTPS, Let's Encrypt TLS-ALPN-01)"},
			[2]string{"ports", ports},
			[2]string{"login", "panel account: password (argon2id, per-IP failure limit) + TOTP"})
		p.Warnings = append(p.Warnings,
			"the panel leaves the SSH layer behind: anyone on the Internet can reach its login page, so only the password + TOTP protect it; review the logins with `lazylongarm hub panel-user --list`",
			"public_url also becomes the base URL of the hub's install scripts and links")
		if handEdited {
			p.Warnings = append(p.Warnings, hubhost.ConfigPath+" has keys or comments lazylongarm does not manage; the rewrite drops them (the previous file is kept as hub.toml.bak-*)")
		}
		nsg := panelNSG(h)
		if nsg == "" {
			p.Warnings = append(p.Warnings, "no cloud firewall is recorded for this hub: open "+ports+" yourself before the HTTPS check")
		}
		p.Steps = append(p.Steps, bindCapabilityStep(host), hubConfigStep(host, target), restartHubStep(host, panel))
		if nsg != "" {
			p.Steps = append(p.Steps, openPanelRulesStep(nsg, cloudfw.PanelRules(req.Redirect), deps.Rules[nsg]))
			if !req.Redirect {
				p.Steps = append(p.Steps, closePanelRulesStep(nsg, []cloudfw.Want{cloudfw.PanelRules(true)[1]}))
			}
		}
		p.Steps = append(p.Steps, httpsReachableStep(url, deps))
	}

	p.Steps = append(p.Steps, plan.Step{
		ID: "save-panel-access", Title: "Record the panel access in lazylongarm's config", Idempotent: true,
		Actions: []plan.Action{{Where: "local", Display: "save panel access " + req.Mode + " for hub " + h.ID}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			if deps.Config == nil {
				return nil
			}
			// Re-read under the lock so records other commands made meanwhile
			// are kept.
			if err := deps.Config.Update(func(cfg *config.Config) error {
				stored, ok := cfg.Hub(h.ID)
				if !ok {
					return fmt.Errorf("hub %s is no longer in %s", h.ID, cfg.Path())
				}
				hub := *stored
				switch req.Mode {
				case config.PanelLoopback:
					hub.PanelAccess = nil
				case config.PanelTailnet:
					hub.PanelAccess = &config.PanelAccess{Mode: config.PanelTailnet, URL: env.Get("panel_url")}
				case config.PanelPublic:
					hub.PanelAccess = &config.PanelAccess{Mode: config.PanelPublic, URL: url, Domain: req.Domain, ACMEEmail: req.ACMEEmail, Redirect: req.Redirect}
				}
				if hub.Cloud != nil {
					c := *hub.Cloud
					c.Rules = nil
					for _, r := range hub.Cloud.Rules {
						if !strings.HasPrefix(r, cloudfw.RulePrefix+"panel-") {
							c.Rules = append(c.Rules, r)
						}
					}
					if req.Mode == config.PanelPublic && panelNSG(h) != "" {
						c.Rules = append(c.Rules, cloudfw.Names(cloudfw.PanelRules(req.Redirect))...)
					}
					hub.Cloud = &c
				}
				cfg.UpsertHub(hub)
				return nil
			}); err != nil {
				return err
			}
			env.Logf("  saved panel access %q for hub %s", req.Mode, h.ID)
			return nil
		},
	})
	return p, nil
}

func describePanel(mode, url string) string {
	switch mode {
	case config.PanelTailnet:
		return "tailnet: " + orDefault(url, "tailscale serve") + " (plus the SSH tunnel)"
	case config.PanelPublic:
		return "public HTTPS: " + url + " (plus the SSH tunnel)"
	}
	return "loopback only: SSH tunnel (`lazylongarm hub panel`, p in the dashboard)"
}

func tailscaleSummary(f *hostprobe.Facts) string {
	switch {
	case f == nil:
		return "unknown"
	case f.TailscaleIP != "":
		return "installed, tailnet IP " + f.TailscaleIP
	case f.Has["tailscale"]:
		return "installed, not logged in"
	}
	return fmt.Sprintf("not installed: official apt repository for %s %s (signing key %s)", f.DistroID, f.DistroCode, tailscalehost.KeyFingerprint)
}

func hasPanelRules(h *config.Hub) bool {
	if h.Cloud == nil {
		return false
	}
	for _, r := range h.Cloud.Rules {
		if strings.HasPrefix(r, cloudfw.RulePrefix+"panel-") {
			return true
		}
	}
	return false
}

func panelNSG(h *config.Hub) string {
	if h.Cloud != nil && h.Cloud.Provider == "azure" {
		return h.Cloud.NSG
	}
	return ""
}

// ---- tailnet ----

func tailnetSteps(req PanelAccessRequest, f *hostprobe.Facts, host, panel string) []plan.Step {
	run := remote(host)
	quick := func(script string, args ...string) connection.Request {
		r := run(script, args...)
		r.Timeout = 30 * time.Second
		return r
	}
	distro, code := "", ""
	if f != nil {
		distro, code = f.DistroID, f.DistroCode
	}
	target := "http://" + panel
	status := func(ctx context.Context, env *plan.Env) (tailscalehost.Status, error) {
		res, err := env.RunQuiet(ctx, quick(`tailscale status --json 2>/dev/null || true`))
		if err != nil {
			return tailscalehost.Status{}, err
		}
		if len(strings.TrimSpace(string(res.Stdout))) == 0 {
			return tailscalehost.Status{}, nil
		}
		return tailscalehost.ParseStatus(res.Stdout)
	}
	installed := func(ctx context.Context, env *plan.Env) (bool, error) {
		_, err := env.RunQuiet(ctx, quick(`command -v tailscale >/dev/null 2>&1`))
		return checkResult(err)
	}

	repo := aptRepoStep("tailscale-repo", "Add the official Tailscale apt repository (signing key verified)", host,
		tailscalehost.KeyURL(distro, code), tailscalehost.KeyFingerprint, tailscalehost.Keyring, tailscalehost.Sources, tailscalehost.SourcesContent(distro, code))
	repoCheck := repo.Check
	repo.Check = func(ctx context.Context, env *plan.Env) (bool, error) {
		if ok, err := installed(ctx, env); ok || err != nil {
			return ok, err
		}
		return repoCheck(ctx, env)
	}
	inst := aptInstallStep("install-tailscale", "Install the tailscale package", host, "tailscale", "apt-get install -y tailscale   (tailscaled.service + the tailscale CLI)")
	inst.Check = installed

	hostname := config.Slug(req.Hub.Name)
	if hostname == "" {
		hostname = config.Slug(req.Hub.ID)
	}
	upFlags := `--hostname="$1" --accept-dns=false --accept-routes=false`
	return []plan.Step{repo, inst, {
		ID: "tailscaled", Title: "Enable and start tailscaled", Idempotent: true,
		Actions: []plan.Action{{Where: host, Privileged: true, Display: "systemctl enable --now tailscaled"}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			_, err := env.RunQuiet(ctx, quick(`systemctl is-active --quiet tailscaled`))
			return checkResult(err)
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			_, err := env.RunPrivileged(ctx, run(`systemctl enable --now tailscaled 2>&1`))
			return err
		},
	}, {
		ID: "tailscale-login", Title: "Log the hub host in to your tailnet", Idempotent: true,
		Actions: []plan.Action{{Where: host, Privileged: true, Display: map[bool]string{
			true:  "tailscale up --hostname=" + hostname + " --accept-dns=false --accept-routes=false   (terminal handed over: open the login URL it prints)",
			false: "tailscale up --auth-key=file:<0600 temp file from stdin> --hostname=" + hostname + " --accept-dns=false --accept-routes=false",
		}[req.AuthKey.Empty()] + "\n(--accept-dns=false leaves the hub's resolv.conf alone; no exit node, no subnet routes)"}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			s, err := status(ctx, env)
			if err != nil {
				return false, err
			}
			if s.Running() {
				env.Logf("  already on the tailnet as %s (%s)", s.DNSName(), s.IPv4())
			}
			return s.Running(), nil
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			if !req.AuthKey.Empty() {
				r := run(`set -eu
umask 077
f=$(mktemp)
trap 'rm -f "$f"' EXIT
cat > "$f"
tailscale up --auth-key="file:$f" `+upFlags+` 2>&1`, hostname)
				r.Sudo, r.Stdin, r.Timeout = true, []byte(req.AuthKey.Reveal()+"\n"), 2*time.Minute
				env.SetSecret("tailscale_auth_key", req.AuthKey)
				_, err := env.RunQuiet(ctx, r)
				return err
			}
			if env.Interactive == nil {
				return plan.WithFix(errors.New("logging in to Tailscale needs a terminal (tailscale up prints a login URL)"),
					"rerun in a terminal, or pass --auth-key-file with a pre-approved auth key")
			}
			env.Logf("  tailscale up prints a login URL: open it in a browser (any device), sign in and approve the machine")
			r := run(`tailscale up `+upFlags, hostname)
			r.Sudo, r.Timeout = true, 15*time.Minute
			return env.Interactive(ctx, r)
		},
		Verify: func(ctx context.Context, env *plan.Env) error {
			s, err := status(ctx, env)
			if err != nil {
				return err
			}
			if !s.Running() {
				return plan.WithFix(fmt.Errorf("tailscale is %q, not Running", orDefault(s.BackendState, "unknown")), "ssh "+host+" sudo tailscale up")
			}
			env.Logf("  on the tailnet as %s (%s)", s.DNSName(), s.IPv4())
			return nil
		},
	}, {
		ID: "tailscale-serve", Title: "Serve the panel over HTTPS on the tailnet (tailscale serve, never funnel)", Idempotent: true,
		Actions: []plan.Action{{Where: host, Privileged: true, Display: "tailscale serve --bg --https=443 " + target + "   (tailnet only; the panel itself stays on loopback)"}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			r := quick(`tailscale serve status --json 2>/dev/null || true`)
			r.Sudo = true
			res, err := env.RunQuiet(ctx, r)
			if err != nil {
				return false, err
			}
			return tailscalehost.Serves(res.Stdout, target), nil
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			s, err := status(ctx, env)
			if err != nil {
				return err
			}
			if !s.HTTPSEnabled() {
				return plan.WithFix(errors.New("HTTPS certificates are off for this tailnet"),
					"turn on MagicDNS and HTTPS Certificates at "+tailscalehost.AdminDNS+", then rerun `lazylongarm hub panel-access tailnet`")
			}
			r := run(`tailscale serve --bg --https=443 "$1" 2>&1`, target)
			r.Sudo, r.Timeout = true, 3*time.Minute
			if env.Interactive != nil {
				// serve may print a URL to enable Serve for the tailnet and wait.
				return env.Interactive(ctx, r)
			}
			_, err = env.RunPrivileged(ctx, r)
			return err
		},
	}, {
		ID: "tailnet-url", Title: "Check the panel's tailnet URL", Idempotent: true, ReadOnly: true,
		Actions: []plan.Action{{Where: host, Display: "curl https://<hub>.<tailnet>.ts.net/healthz on the hub (resolved to its tailnet IP; the first request may wait for the certificate)"}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			s, err := status(ctx, env)
			if err != nil {
				return err
			}
			if s.URL() == "" || s.IPv4() == "" {
				return errors.New("tailscale status has no MagicDNS name or tailnet IP for the hub")
			}
			r := run(`i=0; while [ $i -lt 12 ]; do curl -fsS --max-time 10 --resolve "$1:443:$2" "https://$1/healthz" >/dev/null 2>&1 && exit 0; i=$((i+1)); sleep 5; done; exit 1`, s.DNSName(), s.IPv4())
			r.Timeout = 3 * time.Minute
			if _, err := env.RunQuiet(ctx, r); err != nil {
				return plan.WithFix(fmt.Errorf("%shealthz does not answer over HTTPS: %w", s.URL(), err), "ssh "+host+" sudo tailscale serve status")
			}
			env.Set("panel_url", s.URL())
			env.Logf("  panel: %s  (open it on any device in your tailnet)", s.URL())
			return nil
		},
	}}
}

func tailscaleServeOffStep(host string) plan.Step {
	run := remote(host)
	return plan.Step{
		ID: "tailscale-serve-off", Title: "Stop serving the panel on the tailnet", Idempotent: true,
		Actions: []plan.Action{{Where: host, Privileged: true, Display: "tailscale serve --https=443 off   (skipped when nothing is served)"}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			r := run(`command -v tailscale >/dev/null 2>&1 || exit 0; tailscale serve status --json 2>/dev/null || true`)
			r.Sudo, r.Timeout = true, 30*time.Second
			res, err := env.RunQuiet(ctx, r)
			if err != nil {
				return false, err
			}
			return !tailscalehost.ServesHTTPS(res.Stdout), nil
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			_, err := env.RunPrivileged(ctx, run(`tailscale serve --https=443 off 2>&1`))
			return err
		},
	}
}

// ---- public ----

func bindCapabilityStep(host string) plan.Step {
	run := remote(host)
	sum := hubhost.SHA256(hubhost.HTTPSDropInContent)
	return plan.Step{
		ID: "bind-capability", Title: "Let longarm-hub bind ports 443/80 (systemd drop-in)", Idempotent: true,
		Actions: []plan.Action{{Where: host, Privileged: true, Display: "write " + hubhost.HTTPSDropIn + ":\n" + indent(hubhost.HTTPSDropInContent, "  ") + "\nsystemctl daemon-reload\n(the packaged unit runs as longarm-hub without CAP_NET_BIND_SERVICE)"}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			r := run(`[ "$(sha256sum "$1" 2>/dev/null | cut -d' ' -f1)" = "$2" ]`, hubhost.HTTPSDropIn, sum)
			r.Timeout = 30 * time.Second
			_, err := env.RunQuiet(ctx, r)
			return checkResult(err)
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			if _, err := env.RunPrivileged(ctx, run(`set -eu
install -d -m 0755 "$(dirname "$1")"
printf '%s' "$2" > "$1.tmp" && chmod 0644 "$1.tmp" && mv -f "$1.tmp" "$1"
systemctl daemon-reload
echo "wrote $1"`, hubhost.HTTPSDropIn, hubhost.HTTPSDropInContent)); err != nil {
				return err
			}
			env.Set(envHubConfigChanged, "1")
			return nil
		},
	}
}

func removeDropInStep(host string) plan.Step {
	run := remote(host)
	return plan.Step{
		ID: "remove-bind-capability", Title: "Remove the port 443/80 drop-in", Idempotent: true,
		Actions: []plan.Action{{Where: host, Privileged: true, Display: "rm " + hubhost.HTTPSDropIn + "; systemctl daemon-reload   (skipped when absent)"}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			r := run(`! test -e "$1"`, hubhost.HTTPSDropIn)
			r.Timeout = 30 * time.Second
			_, err := env.RunQuiet(ctx, r)
			return checkResult(err)
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			if _, err := env.RunPrivileged(ctx, run(`rm -f "$1" && systemctl daemon-reload`, hubhost.HTTPSDropIn)); err != nil {
				return err
			}
			env.Set(envHubConfigChanged, "1")
			return nil
		},
	}
}

func openPanelRulesStep(nsg string, wants []cloudfw.Want, planned []cloudfw.Planned) plan.Step {
	ref, _ := cloudfw.ParseNSG(nsg)
	var displays []string
	for _, pr := range planned {
		displays = append(displays, cloudfw.DisplayCommand(cloudfw.CreateArgs(ref, pr)))
	}
	if len(displays) == 0 {
		displays = append(displays, "(rules already present or planned at apply time: "+strings.Join(cloudfw.Names(wants), ", ")+")")
	}
	return plan.Step{
		ID: "panel-firewall", Title: "Open the panel ports in the Azure network security group", Idempotent: true,
		Actions: []plan.Action{{Where: "azure", Display: strings.Join(displays, "\n")}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			rules, err := cloudfw.ListRules(ctx, nsg)
			if err != nil {
				return false, err
			}
			missing, err := cloudfw.PlanRules(rules, wants)
			return len(missing) == 0, err
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			ref, err := cloudfw.ParseNSG(nsg)
			if err != nil {
				return err
			}
			rules, err := cloudfw.ListRules(ctx, nsg)
			if err != nil {
				return err
			}
			missing, err := cloudfw.PlanRules(rules, wants)
			if err != nil {
				return err
			}
			for _, pr := range missing {
				env.Logf("  creating %s (priority %d, %s %s) on %s", pr.Name, pr.Priority, pr.Protocol, pr.Ports, ref.Name)
				if err := cloudfw.Create(ctx, ref, pr); err != nil {
					return &plan.UnknownError{Err: err}
				}
			}
			return nil
		},
	}
}

func closePanelRulesStep(nsg string, wants []cloudfw.Want) plan.Step {
	names := cloudfw.Names(wants)
	present := func(ctx context.Context) ([]string, error) {
		rules, err := cloudfw.ListRules(ctx, nsg)
		if err != nil {
			return nil, err
		}
		want := map[string]bool{}
		for _, n := range names {
			want[n] = true
		}
		var out []string
		for _, r := range rules {
			if want[r.Name] {
				out = append(out, r.Name)
			}
		}
		return out, nil
	}
	id := "panel-firewall-close"
	if len(names) == 1 {
		id = "panel-http-close"
	}
	return plan.Step{
		ID: id, Title: "Remove lazylongarm's panel rules from the Azure network security group", Idempotent: true,
		Actions: []plan.Action{{Where: "azure", Display: "az network nsg rule delete … --name " + strings.Join(names, " / ") + "   (only those present)"}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			have, err := present(ctx)
			return len(have) == 0, err
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			ref, err := cloudfw.ParseNSG(nsg)
			if err != nil {
				return err
			}
			have, err := present(ctx)
			if err != nil {
				return err
			}
			for _, n := range have {
				env.Logf("  deleting %s on %s", n, ref.Name)
				if err := cloudfw.Delete(ctx, ref, n); err != nil {
					return &plan.UnknownError{Err: err}
				}
			}
			return nil
		},
	}
}

func httpsReachableStep(url string, deps PanelAccessDeps) plan.Step {
	return plan.Step{
		ID: "https-reachable", Title: "Open " + url + "healthz from this machine", Idempotent: true, ReadOnly: true,
		Actions: []plan.Action{{Where: "local", Display: "GET " + url + "healthz with certificate verification (retries for up to 2 min while Let's Encrypt issues the certificate)"}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			get := deps.Get
			if get == nil {
				get = HTTPSGet
			}
			deadline := time.Now().Add(2 * time.Minute)
			var err error
			for attempt := 0; time.Now().Before(deadline); attempt++ {
				if err = get(ctx, url+"healthz"); err == nil {
					env.Logf("  %s answers with a valid certificate", url)
					return nil
				}
				if attempt == 0 {
					env.Logf("  not ready yet (%v); retrying", err)
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(5 * time.Second):
				}
			}
			return plan.WithFix(fmt.Errorf("cannot open %s: %w", url, err), "check that 443/tcp is open to the Internet and the DNS name points at the hub; ssh <hub> sudo journalctl -u longarm-hub -n 50 | grep -i acme")
		},
	}
}

// HTTPSGet fetches url with certificate verification and fails on non-2xx.
func HTTPSGet(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, Proxy: http.ProxyFromEnvironment}}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
