// Package flows builds the reviewed plans behind lazylongarm's wizards and
// commands: hub setup, client enrollment and worker add. A flow turns a
// Request plus probed host facts into a plan.Plan; CLI flags, the wizard and
// the dashboard all go through the same Build functions.
package flows

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/cloudfw"
	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/hostprobe"
	"github.com/daviddwlee84/lazylongarm/internal/hubhost"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/secret"
)

// HubRequest is everything `hub setup` needs; zero values take defaults.
type HubRequest struct {
	Host        string // SSH alias of the hub machine (required)
	Mode        string // config.ModePublic | config.ModePrivate
	Address     string // DNS name or IP nodes dial (no port)
	Port        int    // default 7443
	Panel       string // default 127.0.0.1:7480
	AutoApprove bool
	RelayRate   string
	Cloud       string // "azure" | "" (skip)
	EnrollLocal bool   // also enroll this machine as the admin client
	Name        string // hub id/name in lazylongarm config (default: Host)
	Version     string // Longarm release tag used for local/worker binaries
}

// HubDeps are the side effects a hub plan needs, injectable for tests.
type HubDeps struct {
	Config  *config.Config
	Locals  *LocalDeps
	Dial    func(ctx context.Context, address string) error
	AzureVM *cloudfw.AzureVM // resolved during review when Cloud == "azure"
	Rules   map[string][]cloudfw.Planned
}

// LocalDeps install and drive the local longarm client.
type LocalDeps struct {
	CacheDir string
	Client   LongarmClient
	Install  func(ctx context.Context, env *plan.Env, tag string) (string, error)
}

// HubDefaults derives a request from probed facts.
func HubDefaults(req HubRequest, f *hostprobe.Facts) HubRequest {
	if req.Port == 0 {
		req.Port = 7443
	}
	if req.Panel == "" {
		req.Panel = "127.0.0.1:7480"
	}
	if req.RelayRate == "" {
		req.RelayRate = "32M"
	}
	if req.Name == "" {
		req.Name = DefaultHubName(req.Host)
	}
	if req.Mode == "" {
		if f != nil && f.TailscaleIP != "" && req.Address == f.TailscaleIP {
			req.Mode = config.ModePrivate
		} else {
			req.Mode = config.ModePublic
		}
	}
	return req
}

// DefaultHubName turns an SSH destination into a short hub name:
// "admin@hub-1.example.com" → "hub-1", "my-vps" → "my-vps".
func DefaultHubName(host string) string {
	if _, h, ok := strings.Cut(host, "@"); ok {
		host = h
	}
	if net.ParseIP(host) == nil {
		host, _, _ = strings.Cut(host, ".")
	}
	if s := config.Slug(host); s != "" {
		return s
	}
	return "hub"
}

// ValidateHub checks a complete request against the probed host.
func ValidateHub(req HubRequest, f *hostprobe.Facts) error {
	var problems []string
	if req.Host == "" {
		problems = append(problems, "an SSH host for the hub is required")
	}
	if req.Address == "" {
		problems = append(problems, "the address nodes dial (DNS name or IP) is required")
	} else if strings.ContainsAny(req.Address, " /:'\"") {
		problems = append(problems, "address must be a bare DNS name or IP (the port is separate)")
	}
	if req.Mode != config.ModePublic && req.Mode != config.ModePrivate {
		problems = append(problems, "mode must be public or private")
	}
	if req.Port < 1 || req.Port > 65534 {
		problems = append(problems, "port must be 1-65534")
	}
	if req.Cloud != "" && req.Cloud != "azure" {
		problems = append(problems, "cloud firewall provider must be azure or empty")
	}
	if f != nil {
		if f.OS != "linux" || (f.DistroID != "ubuntu" && f.DistroID != "debian" && !f.Has["apt-get"]) {
			problems = append(problems, fmt.Sprintf("hub setup supports Debian/Ubuntu (apt) hosts for now; %s is %s", f.Where(), f.Distro))
		}
		if f.Arch != "amd64" && f.Arch != "arm64" {
			problems = append(problems, "unsupported CPU architecture "+f.RawArch)
		}
		if f.Sudo == "absent" {
			problems = append(problems, "sudo is not available on "+f.Where())
		}
		if !f.Has["systemctl"] {
			problems = append(problems, "systemd is required on the hub host")
		}
		if !f.Has["curl"] {
			problems = append(problems, "curl is required on the hub host")
		}
		if !f.Has["gpg"] {
			problems = append(problems, "gpg is required to verify the apt signing key")
		}
		hubRunning := f.Services["longarm_hub"] == "active"
		if !hubRunning {
			for _, p := range []string{"tcp/" + strconv.Itoa(req.Port), "udp/" + strconv.Itoa(req.Port), "udp/" + strconv.Itoa(req.Port+1)} {
				if addr, busy := f.Ports()[p]; busy {
					problems = append(problems, fmt.Sprintf("%s is already in use on %s (%s)", p, f.Where(), addr))
				}
			}
			if _, port, err := net.SplitHostPort(req.Panel); err == nil {
				if addr, busy := f.Ports()["tcp/"+port]; busy {
					problems = append(problems, fmt.Sprintf("panel port tcp/%s is already in use (%s)", port, addr))
				}
			}
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// HubSettings converts a request into hub.toml settings.
func HubSettings(req HubRequest) hubhost.Settings {
	return hubhost.Settings{
		Port:        req.Port,
		Stun2:       req.Port + 1,
		Panel:       req.Panel,
		PublicAddr:  net.JoinHostPort(req.Address, strconv.Itoa(req.Port)),
		Hosts:       []string{req.Address},
		AutoApprove: req.AutoApprove,
		RelayRate:   req.RelayRate,
	}.Defaults()
}

// HubAddress is host:port nodes dial.
func (r HubRequest) HubAddress() string { return net.JoinHostPort(r.Address, strconv.Itoa(r.Port)) }

// BuildHubSetup assembles the hub setup plan.
func BuildHubSetup(req HubRequest, f *hostprobe.Facts, deps HubDeps) (*plan.Plan, error) {
	req = HubDefaults(req, f)
	if err := ValidateHub(req, f); err != nil {
		return nil, err
	}
	settings := HubSettings(req)
	content := hubhost.Render(settings)
	host := req.Host
	p := &plan.Plan{
		Kind:  "hub-setup",
		Key:   host,
		Title: fmt.Sprintf("Set up longarm-hub on %s (%s)", host, f.Distro),
		Summary: [][2]string{
			{"hub host", fmt.Sprintf("%s · %s · %d CPU · %s", host, f.Platform(), f.CPUs, f.MemSummary())},
			{"mode", req.Mode},
			{"nodes dial", settings.PublicAddr},
			{"ports", fmt.Sprintf("%d/tcp (gRPC mTLS + relay TLS), %d/udp (QUIC), %d/udp (STUN)", settings.Port, settings.Port, settings.Stun2)},
			{"panel", settings.Panel + " (loopback; open with `lazylongarm hub panel`)"},
			{"approval", map[bool]string{true: "auto-approve joining workers", false: "admin approval (owned workers join pre-approved)"}[req.AutoApprove]},
			{"install", "official apt repository + systemd unit (no docker)"},
		},
	}
	if req.Cloud == "azure" && deps.AzureVM != nil {
		p.Summary = append(p.Summary, [2]string{"cloud firewall", fmt.Sprintf("Azure VM %s (%s), %d NSG(s)", deps.AzureVM.Name, deps.AzureVM.ResourceGroup, len(deps.AzureVM.NSGs))})
	}
	if req.EnrollLocal {
		p.Summary = append(p.Summary, [2]string{"this machine", "enroll as the hub's admin client"})
	}
	if f.MemAvailKB > 0 && f.MemAvailKB < 400*1024 {
		p.Warnings = append(p.Warnings, fmt.Sprintf("only %s free on %s; longarm-hub needs ~100-300 MB", f.MemSummary(), host))
	}
	for _, c := range f.Containers {
		lc := strings.ToLower(c)
		if strings.Contains(lc, "squid") || strings.Contains(lc, "v2ray") || strings.Contains(lc, "xray") || strings.Contains(lc, "frp") {
			p.Warnings = append(p.Warnings, fmt.Sprintf("proxy %q runs on this host: make sure it cannot reach 127.0.0.1 (the panel) on behalf of its clients", c))
			break
		}
	}
	if req.Mode == config.ModePublic {
		p.Warnings = append(p.Warnings, "port "+strconv.Itoa(req.Port)+" will be reachable from the Internet; only mTLS nodes and one-time join codes are accepted")
	}
	if req.Mode == config.ModePublic {
		p.Warnings = append(p.Warnings, "if this machine routes traffic through a proxy TUN (e.g. Clash/mihomo), add a DIRECT rule for "+req.Address+" or the reachability check may lie")
	}

	run := func(script string, args ...string) connection.Request {
		return connection.Request{Host: host, Script: script, Args: args, Timeout: 5 * time.Minute}
	}
	keyScript := `set -eu
install -d -m 0755 /etc/apt/keyrings
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
curl -fsSL "$1" -o "$tmp"
fpr=$(gpg --show-keys --with-colons "$tmp" 2>/dev/null | awk -F: '/^fpr:/{print $10; exit}')
if [ "$fpr" != "$2" ]; then echo "unexpected apt signing key fingerprint: $fpr" >&2; exit 1; fi
install -m 0644 "$tmp" "$3"
printf '%s' "$5" > "$4.tmp" && chmod 0644 "$4.tmp" && mv "$4.tmp" "$4"
echo "repository key $fpr verified"
DEBIAN_FRONTEND=noninteractive apt-get update -qq
echo "apt index updated"`
	p.Steps = append(p.Steps, plan.Step{
		ID: "apt-repo", Title: "Add the official Longarm apt repository (signing key verified)", Idempotent: true,
		Actions: []plan.Action{{Where: host, Privileged: true, Display: fmt.Sprintf("curl %s → verify fingerprint %s → %s\nwrite %s; apt-get update", hubhost.AptKeyURL, hubhost.AptKeyFingerprint, hubhost.AptKeyring, hubhost.AptSources)}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			r := run(`test -s "$1" && test -s "$2"`, hubhost.AptKeyring, hubhost.AptSources)
			r.Timeout = 30 * time.Second
			_, err := env.RunQuiet(ctx, r)
			return checkResult(err)
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			r := run(keyScript, hubhost.AptKeyURL, hubhost.AptKeyFingerprint, hubhost.AptKeyring, hubhost.AptSources, hubhost.AptSourcesContent())
			_, err := env.RunPrivileged(ctx, r)
			return err
		},
	})
	p.Steps = append(p.Steps, plan.Step{
		ID: "install-hub", Title: "Install the longarm-hub package", Idempotent: true,
		Actions: []plan.Action{{Where: host, Privileged: true, Display: "apt-get install -y longarm-hub   (creates user longarm-hub, unit longarm-hub.service)"}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			r := run(`dpkg-query -W -f='${Status}' longarm-hub 2>/dev/null | grep -q 'install ok installed'`)
			r.Timeout = 30 * time.Second
			_, err := env.RunQuiet(ctx, r)
			return checkResult(err)
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			_, err := env.RunPrivileged(ctx, run(`DEBIAN_FRONTEND=noninteractive apt-get install -y -q longarm-hub`))
			return err
		},
		Verify: func(ctx context.Context, env *plan.Env) error {
			res, err := env.RunQuiet(ctx, run(`dpkg-query -W -f='${Version}' longarm-hub`))
			if err == nil {
				env.Set("hub_version", strings.TrimSpace(string(res.Stdout)))
				env.Logf("  longarm-hub %s installed", env.Get("hub_version"))
			}
			return err
		},
	})

	p.Steps = append(p.Steps, plan.Step{
		ID: "data-dir", Title: "Create the hub data directory owned by longarm-hub", Idempotent: true,
		Actions: []plan.Action{{Where: host, Privileged: true, Display: "install -d -m 0700 -o longarm-hub -g longarm-hub " + hubhost.DataDir}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			r := run(`test -d "$1" && [ "$(stat -c %U "$1")" = "$2" ]`, hubhost.DataDir, hubhost.ServiceUser)
			r.Timeout = 30 * time.Second
			_, err := env.RunQuiet(ctx, r)
			return checkResult(err)
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			_, err := env.RunPrivileged(ctx, run(`install -d -m 0700 -o "$2" -g "$2" "$1"`, hubhost.DataDir, hubhost.ServiceUser))
			return err
		},
	})

	body, delim := hubhost.Heredoc(content)
	sum := hubhost.SHA256(content)
	writeScript := `set -eu
cfg="$1"
if [ -f "$cfg" ]; then cp -p "$cfg" "$cfg.bak-$(date +%Y%m%d%H%M%S)"; echo "backed up the previous $cfg"; fi
umask 027
cat > "$cfg.lazylongarm.tmp" <<'` + delim + `'
` + body + delim + `
chown root:longarm-hub "$cfg.lazylongarm.tmp"
chmod 0640 "$cfg.lazylongarm.tmp"
mv -f "$cfg.lazylongarm.tmp" "$cfg"
echo "wrote $cfg"`
	p.Steps = append(p.Steps, plan.Step{
		ID: "hub-config", Title: "Write " + hubhost.ConfigPath, Idempotent: true,
		Actions: []plan.Action{{Where: host, Privileged: true, Display: "write " + hubhost.ConfigPath + " (root:longarm-hub 0640, previous file backed up):\n" + indent(content, "  ")}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			r := run(`[ "$(sha256sum "$1" 2>/dev/null | cut -d' ' -f1)" = "$2" ]`, hubhost.ConfigPath, sum)
			r.Sudo, r.Timeout = true, 30*time.Second
			_, err := env.RunQuiet(ctx, r)
			return checkResult(err)
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			_, err := env.RunPrivileged(ctx, run(writeScript, hubhost.ConfigPath))
			return err
		},
	})

	p.Steps = append(p.Steps, plan.Step{
		ID: "bootstrap", Title: "Create the hub CA and a one-time admin join code (longarm-hub bootstrap)", Idempotent: true,
		Actions: []plan.Action{{Where: host, Privileged: true, Display: "runuser -u longarm-hub -- longarm-hub --config " + hubhost.ConfigPath + " bootstrap --admin-code-ttl 15m\n(as the service user, so the state dir stays owned by it)"}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			r := run(`test -s "$1/ca/ca.crt" && systemctl is-active --quiet longarm-hub`, hubhost.DataDir)
			r.Sudo, r.Timeout = true, 30*time.Second
			_, err := env.RunQuiet(ctx, r)
			return checkResult(err)
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			_, err := env.RunPrivileged(ctx, run(`set -eu
runuser -u longarm-hub -- "$1" --config "$2" bootstrap --admin-code-ttl 15m >/dev/null
echo "CA ready; admin join code written to $3 (0600)"`, hubhost.Binary, hubhost.ConfigPath, hubhost.Bootstrap))
			return err
		},
	})

	p.Steps = append(p.Steps, plan.Step{
		ID: "service", Title: "Enable and start longarm-hub.service", Idempotent: true,
		Actions: []plan.Action{{Where: host, Privileged: true, Display: "systemctl enable --now longarm-hub; wait for http://" + settings.Panel + "/healthz"}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			r := run(`systemctl is-enabled --quiet longarm-hub && systemctl is-active --quiet longarm-hub`)
			r.Timeout = 30 * time.Second
			_, err := env.RunQuiet(ctx, r)
			return checkResult(err)
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			_, err := env.RunPrivileged(ctx, run(`systemctl enable --now longarm-hub 2>&1`))
			return err
		},
		Verify: func(ctx context.Context, env *plan.Env) error {
			r := run(`i=0; while [ $i -lt 30 ]; do out=$(curl -fsS --max-time 2 "http://$1/healthz" 2>/dev/null) && { printf '%s\n' "$out"; exit 0; }; i=$((i+1)); sleep 1; done; systemctl status --no-pager -n 20 longarm-hub >&2 || true; exit 1`, settings.Panel)
			r.Timeout = 60 * time.Second
			res, err := env.RunQuiet(ctx, r)
			if err != nil {
				return plan.WithFix(fmt.Errorf("longarm-hub did not become healthy: %w", err), "ssh "+host+" sudo journalctl -u longarm-hub -n 50")
			}
			env.Logf("  healthz: %s", hubhost.HealthSummary(string(res.Stdout)))
			return nil
		},
	})

	p.Steps = append(p.Steps, plan.Step{
		ID: "fingerprint", Title: "Read the hub CA fingerprint", Idempotent: true, ReadOnly: true,
		Actions: []plan.Action{{Where: host, Privileged: true, Display: "runuser -u longarm-hub -- longarm-hub --config " + hubhost.ConfigPath + " ca show"}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			r := run(`runuser -u longarm-hub -- "$1" --config "$2" ca show`, hubhost.Binary, hubhost.ConfigPath)
			r.Sudo = true
			res, err := env.RunQuiet(ctx, r)
			if err != nil {
				return err
			}
			fp := hubhost.ParseFingerprint(string(res.Stdout))
			if fp == "" {
				return errors.New("could not parse the CA fingerprint from `longarm-hub ca show`")
			}
			env.Set("ca_fingerprint", fp)
			env.Logf("  CA fingerprint %s", fp)
			return nil
		},
	})

	baseline := f.Ports()
	p.Steps = append(p.Steps, plan.Step{
		ID: "ports", Title: "Check listeners: hub ports up, existing services untouched", Idempotent: true, ReadOnly: true,
		Actions: []plan.Action{{Where: host, Display: fmt.Sprintf("ss -lntu: expect %d/tcp %d/udp %d/udp; the %d previously listening sockets must remain", settings.Port, settings.Port, settings.Stun2, len(baseline))}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			r := run(`ss -H -lntu | awk '{print $1" "$5}' | sort -u`)
			r.Timeout = 30 * time.Second
			res, err := env.RunQuiet(ctx, r)
			if err != nil {
				return err
			}
			now := &hostprobe.Facts{}
			for _, l := range strings.Split(string(res.Stdout), "\n") {
				if strings.TrimSpace(l) != "" {
					now.Listening = append(now.Listening, strings.TrimSpace(l))
				}
			}
			after := now.Ports()
			var missing []string
			for _, want := range []string{fmt.Sprintf("tcp/%d", settings.Port), fmt.Sprintf("udp/%d", settings.Port), fmt.Sprintf("udp/%d", settings.Stun2)} {
				if _, ok := after[want]; !ok {
					missing = append(missing, want)
				}
			}
			var lost []string
			for k := range baseline {
				if _, ok := after[k]; !ok {
					lost = append(lost, k)
				}
			}
			if len(lost) > 0 {
				env.Logf("  ! no longer listening (was before setup): %s", strings.Join(lost, ", "))
			}
			if len(missing) > 0 {
				return fmt.Errorf("hub is not listening on %s", strings.Join(missing, ", "))
			}
			env.Logf("  %d/tcp %d/udp %d/udp listening; %d of %d earlier sockets still up", settings.Port, settings.Port, settings.Stun2, len(baseline)-len(lost), len(baseline))
			return nil
		},
	})

	if req.Cloud == "azure" && deps.AzureVM != nil {
		vm := deps.AzureVM
		var displays []string
		for _, nsg := range vm.NSGs {
			ref, _ := cloudfw.ParseNSG(nsg)
			for _, pr := range deps.Rules[nsg] {
				displays = append(displays, cloudfw.DisplayCommand(cloudfw.CreateArgs(ref, pr)))
			}
		}
		if len(displays) == 0 {
			displays = append(displays, "(rules already present: "+strings.Join(ruleNames(settings), ", ")+")")
		}
		p.Steps = append(p.Steps, plan.Step{
			ID: "cloud-firewall", Title: "Open the hub ports in the Azure network security group", Idempotent: true,
			Actions: []plan.Action{{Where: "azure", Display: strings.Join(displays, "\n")}},
			Check: func(ctx context.Context, env *plan.Env) (bool, error) {
				for _, nsg := range vm.NSGs {
					rules, err := cloudfw.ListRules(ctx, nsg)
					if err != nil {
						return false, err
					}
					missing, err := cloudfw.PlanRules(rules, cloudfw.HubRules(settings.Port, settings.Stun2))
					if err != nil {
						return false, err
					}
					if len(missing) > 0 {
						return false, nil
					}
				}
				return true, nil
			},
			Apply: func(ctx context.Context, env *plan.Env) error {
				for _, nsg := range vm.NSGs {
					ref, err := cloudfw.ParseNSG(nsg)
					if err != nil {
						return err
					}
					rules, err := cloudfw.ListRules(ctx, nsg)
					if err != nil {
						return err
					}
					missing, err := cloudfw.PlanRules(rules, cloudfw.HubRules(settings.Port, settings.Stun2))
					if err != nil {
						return err
					}
					for _, pr := range missing {
						env.Logf("  creating %s (priority %d, %s %s) on %s", pr.Name, pr.Priority, pr.Protocol, pr.Ports, ref.Name)
						if err := cloudfw.Create(ctx, ref, pr); err != nil {
							return &plan.UnknownError{Err: err}
						}
					}
				}
				return nil
			},
		})
	}

	address := settings.PublicAddr
	p.Steps = append(p.Steps, plan.Step{
		ID: "reachability", Title: "Dial " + address + " from this machine", Idempotent: true, ReadOnly: true,
		Actions: []plan.Action{{Where: "local", Display: "TCP connect " + address + " (retries for up to 60 s while the firewall applies)"}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			dial := deps.Dial
			if dial == nil {
				dial = DialTCP
			}
			deadline := time.Now().Add(60 * time.Second)
			var err error
			for attempt := 0; time.Now().Before(deadline); attempt++ {
				if err = dial(ctx, address); err == nil {
					env.Logf("  connected to %s", address)
					return nil
				}
				if attempt == 0 {
					env.Logf("  not reachable yet (%v); retrying", err)
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(3 * time.Second):
				}
			}
			return plan.WithFix(fmt.Errorf("cannot reach %s from this machine: %w", address, err), "check the cloud firewall / NSG for "+strconv.Itoa(settings.Port)+"/tcp, and proxy TUN rules on this machine")
		},
	})

	hubID := config.Slug(req.Name)
	if hubID == "" {
		hubID = "hub"
	}
	saveHub := func(env *plan.Env) error {
		if deps.Config == nil {
			return nil
		}
		h := config.Hub{ID: hubID, Name: req.Name, Mode: req.Mode, Address: address, CAFingerprint: env.Get("ca_fingerprint"), SSHHost: host, Panel: settings.Panel, Version: req.Version}
		if old, ok := deps.Config.Hub(hubID); ok && old.ID == hubID {
			h.CreatedAt = old.CreatedAt
			h.ConfigDir = old.ConfigDir
			if h.Version == "" {
				h.Version = old.Version
			}
		}
		if req.Cloud == "azure" && deps.AzureVM != nil {
			h.Cloud = &config.Cloud{Provider: "azure", Subscription: deps.AzureVM.Subscription, ResourceGroup: deps.AzureVM.ResourceGroup, VM: deps.AzureVM.Name, Rules: ruleNames(settings)}
			if len(deps.AzureVM.NSGs) > 0 {
				h.Cloud.NSG = deps.AzureVM.NSGs[0]
			}
		}
		deps.Config.UpsertHub(h)
		if err := deps.Config.Save(); err != nil {
			return err
		}
		env.Set("hub_id", hubID)
		env.Logf("  saved hub %q to %s", hubID, deps.Config.Path())
		return nil
	}
	p.Steps = append(p.Steps, plan.Step{
		ID: "save-hub", Title: "Record the hub in lazylongarm's config", Idempotent: true,
		Actions: []plan.Action{{Where: "local", Display: "save hub profile (address, CA fingerprint, ssh host) to the lazylongarm config"}},
		Apply:   func(ctx context.Context, env *plan.Env) error { return saveHub(env) },
	})

	if req.EnrollLocal && deps.Locals != nil {
		p.Steps = append(p.Steps, clientEnrollSteps(req, host, address, deps)...)
	}
	return p, nil
}

func ruleNames(s hubhost.Settings) []string {
	var out []string
	for _, w := range cloudfw.HubRules(s.Port, s.Stun2) {
		out = append(out, w.Name)
	}
	return out
}

// clientEnrollSteps install longarm locally and join it as the admin client.
func clientEnrollSteps(req HubRequest, host, address string, deps HubDeps) []plan.Step {
	locals := deps.Locals
	tag := req.Version
	steps := []plan.Step{{
		ID: "local-longarm", Title: "Install the longarm client on this machine (~/.local/bin, no sudo)", Idempotent: true,
		Actions: []plan.Action{{Where: "local", Display: fmt.Sprintf("download longarm + longarmd %s from GitHub, verify SHA256SUMS, install to ~/.local/bin (skipped when longarm is already installed)", orLatest(tag))}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			if locals.Client == nil {
				return false, nil
			}
			v, err := locals.Client.Version(ctx)
			if err != nil {
				return false, nil
			}
			env.Logf("  longarm %s already installed", v.Version)
			return true, nil
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			path, err := locals.Install(ctx, env, tag)
			if err != nil {
				return err
			}
			env.Logf("  installed %s", path)
			return nil
		},
	}, {
		ID: "enroll-client", Title: "Join this machine to the hub as its admin client", Idempotent: false,
		Actions: []plan.Action{
			{Where: host, Privileged: true, Display: "read the one-time admin code from " + hubhost.Bootstrap + " (or mint one: longarm-hub join-code --role admin --ttl 15m)"},
			{Where: "local", Display: "longarm join " + address + " <code> --ca-fingerprint <fingerprint>; longarm daemon start"},
		},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			if locals.Client == nil {
				return false, nil
			}
			w, err := locals.Client.WhoAmI(ctx)
			if err != nil {
				return false, nil
			}
			if w.Role != "admin" {
				return false, fmt.Errorf("this machine is already enrolled with a hub as %q (%s); leave it first (longarm leave) or use a separate LONGARM_CONFIG_DIR", w.Role, w.HubName)
			}
			env.Logf("  already enrolled as %s (%s) on hub %q", w.Name, w.Role, w.HubName)
			return true, nil
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			r := connection.Request{Host: host, Script: `cat "$1"`, Args: []string{hubhost.Bootstrap}, Sudo: true, Timeout: 30 * time.Second}
			res, err := env.RunQuiet(ctx, r)
			if err != nil {
				return err
			}
			st := hubhost.ParseBootstrap(string(res.Stdout))
			usable := st.CodeUsable(time.Now())
			code := secret.New(st.AdminCode)
			st.AdminCode = ""
			if !usable {
				env.Logf("  bootstrap code is used or expired; minting a new admin code (15 min)")
				mint := connection.Request{Host: host, Script: `runuser -u longarm-hub -- "$1" --config "$2" join-code --role admin --ttl 15m`, Args: []string{hubhost.Binary, hubhost.ConfigPath}, Sudo: true, Timeout: 30 * time.Second}
				out, err := env.RunQuiet(ctx, mint)
				if err != nil {
					return err
				}
				code = secret.New(strings.TrimSpace(strings.SplitN(string(out.Stdout), "\n", 2)[0]))
			}
			if code.Empty() {
				return errors.New("no admin join code available")
			}
			env.SetSecret("admin_code", code)
			fp := env.Get("ca_fingerprint")
			if fp == "" {
				fp = st.Fingerprint
			}
			out, err := locals.Client.Join(ctx, address, code, fp)
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if strings.TrimSpace(line) != "" {
					env.Logf("  │ %s", line)
				}
			}
			if err != nil {
				return &plan.UnknownError{Err: err}
			}
			if err := locals.Client.DaemonStart(ctx); err != nil {
				env.Logf("  ! longarm daemon start: %v", err)
			}
			return nil
		},
		Verify: func(ctx context.Context, env *plan.Env) error {
			w, err := locals.Client.WhoAmI(ctx)
			if err != nil {
				return err
			}
			if w.Role != "admin" {
				return fmt.Errorf("enrolled as %q, expected admin", w.Role)
			}
			env.Logf("  whoami: %s (%s) on hub %q, certificate valid until %s", w.Name, w.Role, w.HubName, w.CertNotAfter)
			return nil
		},
	}}
	return steps
}

func checkResult(err error) (bool, error) {
	var ce *connection.CommandError
	if errors.As(err, &ce) {
		return false, nil
	}
	return err == nil, err
}

func orLatest(tag string) string {
	if tag == "" {
		return "(latest release)"
	}
	return tag
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// DialTCP connects to address with a short timeout.
func DialTCP(ctx context.Context, address string) error {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	return conn.Close()
}
