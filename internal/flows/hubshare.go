package flows

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/companion"
	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/hubshare"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
)

// ShareRequest describes `hub share`: register the hub this machine
// operates on another operator machine (its longarm already joined).
type ShareRequest struct {
	Host        string      // SSH host of the other machine
	Hub         *config.Hub // the hub to share
	Targets     bool        // also share the managed machines
	NoAuthorize bool        // do not add the machine's key to the hub host
	RemoteBin   string      // lazylongarm on the machine (default: found on its PATH)
	Identity    string      // private key on the machine for the hub host (writes a Host alias there)
	From        string      // this machine's hostname, for the payload
}

// ShareFacts is what `hub share` learned before the review: read-only
// looks at both machines.
type ShareFacts struct {
	Home             string
	Bin              string // lazylongarm on the machine
	BinVersion       string
	HasImport        bool // that lazylongarm has `hub import`
	Membership       hubshare.Membership
	MembershipErr    error
	Local            connection.SSHTarget // the hub's SSH host as this machine resolves it
	Remote           connection.SSHTarget // ... as the machine resolves the same ssh_host
	SSHHost          string               // ssh_host to record there
	HostKeyKnown     bool                 // the machine's known_hosts has the hub host
	KnownHosts       []string             // this machine's known_hosts lines for the hub host
	CanSSH           bool                 // the machine already reaches the hub host (BatchMode)
	PubKey           *hubshare.PublicKey  // the machine's default public key
	PubKeyFile       string
	Includes         []string          // Include patterns of the machine's ~/.ssh/config
	Alias            string            // --identity: the Host alias written there
	AliasFile        string            // ... into this file, which an Include glob picks up
	AliasResolves    bool              // the machine's ssh already resolves Alias to the hub host
	IdentityExists   bool              // the --identity file exists there
	TargetHostnames  map[string]string // target ssh alias → hostname the machine resolves
	LocalTargetHosts map[string]string // ... as this machine resolves it
}

const shareProbeScript = companion.RemotePath + `
kv() { printf '%s=%s\n' "$1" "$2"; }
kv home "$HOME"
d="${LONGARM_CONFIG_DIR:-$HOME/.config/longarm}"
bin="$1"
[ -n "$bin" ] || bin=$(command -v lazylongarm 2>/dev/null || true)
kv lazylongarm "$bin"
if [ -n "$bin" ] && [ -x "$bin" ]; then
  kv lazylongarm_version "$("$bin" version 2>/dev/null | head -n 1)"
  if "$bin" hub import --help 2>&1 | grep -q -- '--stdin'; then kv import yes; fi
fi
ssh -G -- "$2" 2>/dev/null | awk '$1=="hostname"||$1=="user"||$1=="port"||$1=="proxyjump"{print "sshg_" $1 "=" $2}'
awk 'tolower($1)=="include"{for(i=2;i<=NF;i++)print "include=" $i}' "$HOME/.ssh/config" 2>/dev/null
shift 2
for a in "$@"; do kv target "$a $(ssh -G -- "$a" 2>/dev/null | awk '$1=="hostname"{print $2; exit}')"; done
echo '-----CA-----'
cat "$d/tls/ca.crt" 2>/dev/null
echo '-----HUBTOML-----'
cat "$d/hub.toml" 2>/dev/null
exit 0`

// shareAccessScript checks how the machine reaches the hub host $1 whose
// known_hosts name is $2; with an identity file $3, as $1 with that key,
// and whether its ssh resolves the alias $4 yet.
const shareAccessScript = `
kv() { printf '%s=%s\n' "$1" "$2"; }
if ssh-keygen -F "$2" >/dev/null 2>&1; then kv hostkey yes; fi
id="$3"
case "$id" in "~/"*) id="$HOME/${id#\~/}" ;; esac
if [ -n "$id" ]; then
  if [ -f "$id" ]; then kv identity yes; fi
  if [ -f "$id.pub" ]; then kv pubkey_file "$id.pub"; kv pubkey "$(head -n 1 "$id.pub")"; fi
  if ssh -o BatchMode=yes -o ConnectTimeout=8 -o StrictHostKeyChecking=yes -o IdentitiesOnly=yes -i "$id" -- "$1" true >/dev/null 2>&1; then kv can_ssh yes; fi
  kv alias_hostname "$(ssh -G -- "$4" 2>/dev/null | awk '$1=="hostname"{print $2; exit}')"
  exit 0
fi
if ssh -o BatchMode=yes -o ConnectTimeout=8 -o StrictHostKeyChecking=yes -- "$1" true >/dev/null 2>&1; then kv can_ssh yes; fi
for f in $(ssh -G -- "$1" 2>/dev/null | awk '$1=="identityfile"{print $2}'); do
  case "$f" in "~/"*) f="$HOME/${f#\~/}" ;; esac
  if [ -f "$f.pub" ]; then kv pubkey_file "$f.pub"; kv pubkey "$(head -n 1 "$f.pub")"; break; fi
done
exit 0`

func parseKV(b []byte) map[string][]string {
	m := map[string][]string{}
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(l, "=")
		if ok {
			m[k] = append(m[k], v)
		}
	}
	return m
}

func first(m map[string][]string, k string) string {
	if v := m[k]; len(v) > 0 {
		return strings.TrimSpace(v[0])
	}
	return ""
}

// GatherShare looks at the hub host's SSH settings here and at the other
// machine (two read-only ssh round trips).
func GatherShare(ctx context.Context, req ShareRequest, targets []config.Target) (*ShareFacts, error) {
	if req.Hub == nil {
		return nil, errors.New("no hub")
	}
	if req.Hub.SSHHost == "" {
		return nil, fmt.Errorf("hub %s has no SSH host here: nothing to reach it with from %s", req.Hub.ID, req.Host)
	}
	local, err := connection.ResolveSSH(ctx, req.Hub.SSHHost)
	if err != nil {
		return nil, err
	}
	f := &ShareFacts{Local: local, TargetHostnames: map[string]string{}, LocalTargetHosts: map[string]string{}}

	args := []string{req.RemoteBin, req.Hub.SSHHost}
	if req.Targets {
		for _, t := range targets {
			if t.Hub == req.Hub.ID && t.SSHHost != "" && t.SSHHost != req.Host {
				args = append(args, t.SSHHost)
				f.LocalTargetHosts[t.SSHHost] = connection.ResolveHostname(ctx, t.SSHHost)
			}
		}
	}
	res, err := connection.Run(ctx, connection.Request{Host: req.Host, Script: shareProbeScript, Args: args, Timeout: 60 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("look at %s: %w", req.Host, err)
	}
	out := res.Stdout
	kvPart, rest, _ := bytes.Cut(out, []byte("-----CA-----\n"))
	caPart, hubPart, _ := bytes.Cut(rest, []byte("-----HUBTOML-----\n"))
	kv := parseKV(kvPart)
	f.Home, f.Bin, f.BinVersion = first(kv, "home"), first(kv, "lazylongarm"), first(kv, "lazylongarm_version")
	f.HasImport = first(kv, "import") == "yes"
	f.Remote = connection.SSHTarget{Hostname: first(kv, "sshg_hostname"), User: first(kv, "sshg_user"), Port: first(kv, "sshg_port"), ProxyJump: first(kv, "sshg_proxyjump")}
	if f.Remote.ProxyJump == "none" {
		f.Remote.ProxyJump = ""
	}
	for _, t := range kv["target"] {
		alias, host, _ := strings.Cut(t, " ")
		f.TargetHostnames[alias] = strings.TrimSpace(host)
	}
	f.Membership, f.MembershipErr = hubshare.ParseMembership(hubPart, caPart)
	f.Includes = kv["include"]

	// The ssh_host to record there: as is when the machine resolves it to
	// the same host, else this machine's user@hostname (a direct one only).
	f.SSHHost = req.Hub.SSHHost
	accessHost := ""
	if req.Identity != "" {
		// A Host alias with the key, in a file an Include glob of the
		// machine's ~/.ssh/config picks up (never ~/.ssh/config itself).
		if !local.Direct() {
			return f, fmt.Errorf("--identity needs a hub host reachable as user@hostname on port 22 (here: %s, port %s, proxyjump %s)", local.Hostname, dashIf(local.Port), dashIf(local.ProxyJump))
		}
		f.Alias = "lazylongarm-hub-" + req.Hub.ID
		f.AliasFile = PickIncludeFile(f.Includes, f.Home, f.Alias)
		f.SSHHost, accessHost = f.Alias, local.UserHost()
	} else if !sameSSHTarget(local, f.Remote) {
		if !local.Direct() {
			return f, fmt.Errorf("%s resolves the hub's ssh host %q to %s, not %s, and here it needs ProxyJump/another port: add a Host %s entry to %s's ~/.ssh/config, then share again",
				req.Host, req.Hub.SSHHost, dashIf(f.Remote.Hostname), local.Hostname, req.Hub.SSHHost, req.Host)
		}
		f.SSHHost = local.UserHost()
	}

	kh, _ := connection.Run(ctx, connection.Request{Script: `ssh-keygen -F "$1" 2>/dev/null; exit 0`, Args: []string{local.KnownHostsKey()}, Timeout: 10 * time.Second})
	f.KnownHosts = hubshare.KnownHostsLines(kh.Stdout)

	if accessHost == "" {
		accessHost = f.SSHHost
	}
	acc, err := connection.Run(ctx, connection.Request{Host: req.Host, Script: shareAccessScript, Args: []string{accessHost, local.KnownHostsKey(), req.Identity, f.Alias}, Timeout: 40 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("check %s's ssh to the hub host: %w", req.Host, err)
	}
	akv := parseKV(acc.Stdout)
	f.HostKeyKnown = first(akv, "hostkey") == "yes"
	f.CanSSH = first(akv, "can_ssh") == "yes"
	f.PubKeyFile = first(akv, "pubkey_file")
	f.IdentityExists = first(akv, "identity") == "yes"
	f.AliasResolves = f.Alias != "" && strings.EqualFold(first(akv, "alias_hostname"), local.Hostname)
	if line := first(akv, "pubkey"); line != "" {
		if k, err := hubshare.ParsePublicKey(line); err == nil {
			f.PubKey = &k
		}
	}
	return f, nil
}

// PickIncludeFile is where a Host alias file goes so that the machine's
// ~/.ssh/config picks it up: the first Include glob (one with "lazylongarm"
// in it first) whose last path element holds the "*"; "" when none.
func PickIncludeFile(patterns []string, home, name string) string {
	var pick string
	for _, pat := range patterns {
		pat = strings.Trim(strings.TrimSpace(pat), `"`)
		dir, base := "", pat
		if i := strings.LastIndex(pat, "/"); i >= 0 {
			dir, base = pat[:i], pat[i+1:]
		}
		if !strings.Contains(base, "*") || strings.ContainsAny(dir, "*?[") || strings.ContainsAny(base, "?[") {
			continue
		}
		switch {
		case strings.HasPrefix(dir, "~/"):
			dir = home + dir[1:]
		case dir == "~":
			dir = home
		case !strings.HasPrefix(dir, "/"):
			// Relative Include paths are relative to ~/.ssh.
			dir = strings.TrimSuffix(home+"/.ssh/"+dir, "/")
		}
		file := dir + "/" + strings.ReplaceAll(strings.Replace(base, "*", name, 1), "*", "")
		if strings.Contains(pat, "lazylongarm") {
			return file
		}
		if pick == "" {
			pick = file
		}
	}
	return pick
}

func sameSSHTarget(a, b connection.SSHTarget) bool {
	port := func(p string) string {
		if p == "" {
			return "22"
		}
		return p
	}
	return strings.EqualFold(a.Hostname, b.Hostname) && a.User == b.User && port(a.Port) == port(b.Port) && a.ProxyJump == b.ProxyJump
}

func dashIf(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// splitTargets returns the hub's targets as the other machine should
// record them: the machine itself becomes "this machine" there; this
// machine's own entry (no ssh host) and aliases the machine resolves to
// another host (or not at all) are skipped, with a reason.
func splitTargets(f *ShareFacts, req ShareRequest, all []config.Target) (keep []config.Target, skipped []string) {
	for _, t := range all {
		if t.Hub != req.Hub.ID {
			continue
		}
		switch {
		case t.SSHHost == "":
			skipped = append(skipped, fmt.Sprintf("%s (this machine here: on %s add it with its ssh alias)", t.ID, req.Host))
			continue
		case t.SSHHost == req.Host:
			t.SSHHost = "" // the machine itself
		case f != nil:
			there, here := f.TargetHostnames[t.SSHHost], f.LocalTargetHosts[t.SSHHost]
			if there == "" || here == "" || !strings.EqualFold(there, here) {
				skipped = append(skipped, fmt.Sprintf("%s (ssh %s resolves to %s there, %s here)", t.ID, t.SSHHost, dashIf(there), dashIf(here)))
				continue
			}
		}
		keep = append(keep, t)
	}
	return keep, skipped
}

// BuildHubShare turns the gathered facts into the reviewed plan.
func BuildHubShare(req ShareRequest, f *ShareFacts, targets []config.Target) (*plan.Plan, error) {
	h := *req.Hub
	switch {
	case f.MembershipErr != nil:
		return nil, fmt.Errorf("read %s's longarm config: %w", req.Host, f.MembershipErr)
	case !f.Membership.Joined():
		return nil, fmt.Errorf("%s's longarm is not joined to a hub: run `lazylongarm peer add %s` first (or `longarm login %s --ca-fingerprint <sha256>` there)", req.Host, req.Host, h.Address)
	}
	if err := f.Membership.Matches(h); err != nil {
		return nil, fmt.Errorf("%s: %w; refusing to register hub %s there", req.Host, err, h.ID)
	}
	if f.Bin == "" {
		return nil, fmt.Errorf("%s has no lazylongarm on its PATH: install it there (brew install daviddwlee84/tap/lazylongarm, or the release binary), then share again", req.Host)
	}
	if !f.HasImport {
		return nil, fmt.Errorf("%s's lazylongarm (%s, %s) has no `hub import`: upgrade it there (brew upgrade lazylongarm, or `lazylongarm upgrade`), then share again", req.Host, f.Bin, dashIf(f.BinVersion))
	}
	if !f.HostKeyKnown && len(f.KnownHosts) == 0 {
		return nil, fmt.Errorf("this machine's known_hosts has no key for %s, so there is nothing pinned to copy: ssh %s once here (verify the key), then share again", f.Local.KnownHostsKey(), h.SSHHost)
	}
	if req.Identity != "" {
		switch {
		case !f.IdentityExists:
			return nil, fmt.Errorf("%s has no identity file %s", req.Host, req.Identity)
		case f.PubKey == nil:
			return nil, fmt.Errorf("%s has no readable public key %s.pub next to the identity file", req.Host, req.Identity)
		case f.AliasFile == "" && !f.AliasResolves:
			return nil, fmt.Errorf("%s's ~/.ssh/config has no Include glob to drop a Host alias file into (lazylongarm never edits ~/.ssh/config itself): add `Include ~/.ssh/lazylongarm/*.conf` near its top (before any Host block; with chezmoi, in its source), then share again", req.Host)
		}
	}
	if !f.CanSSH && !req.NoAuthorize && f.PubKey == nil {
		return nil, fmt.Errorf("%s has no default SSH key (ssh -G %s lists no identity file with a .pub): create one there (ssh-keygen -t ed25519), then share again; lazylongarm never makes keys", req.Host, f.SSHHost)
	}

	where := req.Host
	shared := h
	shared.SSHHost = f.SSHHost
	kept, skipped := splitTargets(f, req, targets)
	if !req.Targets {
		kept, skipped = nil, nil
	}
	payload := hubshare.NewPayload(shared, kept, req.From)
	body, err := payload.Encode()
	if err != nil {
		return nil, err
	}

	p := &plan.Plan{
		Kind:  "hub-share",
		Key:   req.Host + "-" + h.ID,
		Title: fmt.Sprintf("Share hub %s with %s", h.ID, req.Host),
	}
	p.Summary = append(p.Summary,
		[2]string{"hub", fmt.Sprintf("%s at %s, CA %s", h.ID, h.Address, shortFP(h.CAFingerprint))},
		[2]string{"machine", fmt.Sprintf("%s: its longarm is joined to this hub; lazylongarm %s (%s)", req.Host, dashIf(strings.TrimSpace(strings.TrimPrefix(f.BinVersion, "lazylongarm"))), f.Bin)},
	)
	if f.Alias != "" {
		p.Summary = append(p.Summary, [2]string{"hub ssh host", fmt.Sprintf("%s there: Host alias for %s with IdentityFile %s (file %s)", f.Alias, f.Local.UserHost(), req.Identity, dashIf(f.AliasFile))})
	} else if f.SSHHost != h.SSHHost {
		p.Summary = append(p.Summary, [2]string{"hub ssh host", fmt.Sprintf("%s there (%s has no ssh alias %q resolving to %s)", f.SSHHost, req.Host, h.SSHHost, f.Local.Hostname)})
	} else {
		p.Summary = append(p.Summary, [2]string{"hub ssh host", f.SSHHost})
	}
	if !f.HostKeyKnown {
		p.Summary = append(p.Summary, [2]string{"hub host key", strings.Join(hubshare.KnownHostKeyFingerprints(f.KnownHosts), ", ") + " (pinned from this machine's known_hosts)"})
	}
	if !f.CanSSH && !req.NoAuthorize && f.PubKey != nil {
		p.Summary = append(p.Summary, [2]string{"authorize key", fmt.Sprintf("%s %s %s (%s) on the hub host", f.PubKey.Type, f.PubKey.Fingerprint, f.PubKey.Comment, f.PubKeyFile)})
	}
	if req.Targets {
		p.Summary = append(p.Summary, [2]string{"targets", fmt.Sprintf("%d shared, %d skipped", len(kept), len(skipped))})
		for _, s := range skipped {
			p.Warnings = append(p.Warnings, "target skipped: "+s)
		}
	}
	if h.Cloud != nil {
		p.Warnings = append(p.Warnings, fmt.Sprintf("hub vm / firewall checks on %s need its own `az login` (provider %s)", req.Host, h.Cloud.Provider))
	}
	p.Warnings = append(p.Warnings, "each machine keeps its own copy: after `hub panel-access` or a hub move, share again")

	run := func(script string, args ...string) connection.Request {
		return connection.Request{Host: req.Host, Script: script, Args: args, Timeout: 40 * time.Second}
	}
	khKey := f.Local.KnownHostsKey()

	if f.Alias != "" {
		block := fmt.Sprintf("# Written by `lazylongarm hub share`: the hub host of hub %s.\nHost %s\n  HostName %s\n", h.ID, f.Alias, f.Local.Hostname)
		if f.Local.User != "" {
			block += "  User " + f.Local.User + "\n"
		}
		block += "  IdentityFile " + req.Identity + "\n  IdentitiesOnly yes\n"
		resolves := func(ctx context.Context, env *plan.Env) (bool, error) {
			res, err := env.RunQuiet(ctx, run(`ssh -G -- "$1" 2>/dev/null | awk '$1=="hostname"{print $2; exit}'`, f.Alias))
			if err != nil {
				return false, err
			}
			return strings.EqualFold(strings.TrimSpace(string(res.Stdout)), f.Local.Hostname), nil
		}
		aliasFile := f.AliasFile
		p.Steps = append(p.Steps, plan.Step{
			ID: "hub-ssh-alias", Title: "Host alias " + f.Alias + " on " + req.Host, Idempotent: true,
			Actions: []plan.Action{{Where: where, Display: "write " + dashIf(aliasFile) + ":\n" + strings.TrimSuffix(block, "\n")}},
			Check:   resolves,
			Apply: func(ctx context.Context, env *plan.Env) error {
				if aliasFile == "" {
					return fmt.Errorf("no Include glob to write the alias into")
				}
				r := run(`umask 077; mkdir -p "$(dirname "$1")"; cat > "$1"`, aliasFile)
				r.Stdin = []byte(block)
				_, err := env.Run(ctx, r)
				return err
			},
			Verify: func(ctx context.Context, env *plan.Env) error {
				ok, err := resolves(ctx, env)
				if err != nil {
					return err
				}
				if !ok {
					return plan.WithFix(fmt.Errorf("%s's ssh does not resolve %s to %s", req.Host, f.Alias, f.Local.Hostname),
						"the Include line in its ~/.ssh/config must come before any Host block; check `ssh -G "+f.Alias+"` there")
				}
				return nil
			},
		})
	}

	p.Steps = append(p.Steps, plan.Step{
		ID: "hub-host-key", Title: "Pin the hub host's SSH key on " + req.Host, Idempotent: true,
		Actions: []plan.Action{{Where: where, Display: fmt.Sprintf("append this machine's known_hosts lines for %s to ~/.ssh/known_hosts (missing lines only)", khKey)}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			res, err := env.RunQuiet(ctx, run(`if ssh-keygen -F "$1" >/dev/null 2>&1; then echo yes; else echo no; fi`, khKey))
			if err != nil {
				return false, err
			}
			return strings.TrimSpace(string(res.Stdout)) == "yes", nil
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			req := run(`umask 077; mkdir -p "$HOME/.ssh"; f="$HOME/.ssh/known_hosts"; touch "$f"
while IFS= read -r l; do [ -n "$l" ] || continue; grep -qxF "$l" "$f" || printf '%s\n' "$l" >> "$f"; done`)
			req.Stdin = []byte(strings.Join(f.KnownHosts, "\n") + "\n")
			_, err := env.Run(ctx, req)
			return err
		},
	})

	if !req.NoAuthorize {
		var key hubshare.PublicKey
		if f.PubKey != nil {
			key = *f.PubKey
		}
		canSSH := func(ctx context.Context, env *plan.Env) (bool, error) {
			// The nested ssh's errors go nowhere: its "Permission denied"
			// must not read as this machine failing to reach req.Host.
			res, err := env.RunQuiet(ctx, run(`if ssh -o BatchMode=yes -o ConnectTimeout=8 -o StrictHostKeyChecking=yes -- "$1" true >/dev/null 2>&1; then echo yes; else echo no; fi`, f.SSHHost))
			if err != nil {
				return false, err
			}
			return strings.TrimSpace(string(res.Stdout)) == "yes", nil
		}
		display := fmt.Sprintf("add %s's key %s to ~/.ssh/authorized_keys (only if missing)", req.Host, dashIf(key.Fingerprint))
		p.Steps = append(p.Steps, plan.Step{
			ID: "hub-authorize", Title: req.Host + " can ssh to the hub host", Idempotent: true,
			Actions: []plan.Action{{Where: h.SSHHost, Display: display}},
			Check:   canSSH,
			Apply: func(ctx context.Context, env *plan.Env) error {
				if key.Line == "" {
					return plan.WithFix(fmt.Errorf("%s cannot ssh to %s and has no public key to authorize", req.Host, f.SSHHost), "create one there (ssh-keygen -t ed25519) and share again")
				}
				_, err := env.Run(ctx, connection.Request{Host: h.SSHHost, Timeout: 30 * time.Second, Stdin: []byte(key.Line + "\n"),
					Script: `umask 077; IFS= read -r k; mkdir -p "$HOME/.ssh"; f="$HOME/.ssh/authorized_keys"; touch "$f"; grep -qxF "$k" "$f" || printf '%s\n' "$k" >> "$f"`})
				return err
			},
			Verify: func(ctx context.Context, env *plan.Env) error {
				ok, err := canSSH(ctx, env)
				if err != nil {
					return err
				}
				if !ok {
					return plan.WithFix(fmt.Errorf("%s still cannot ssh to %s", req.Host, f.SSHHost),
						fmt.Sprintf("on %s try `ssh %s true` to see why (another identity file, an agent, sshd's AllowUsers)", req.Host, f.SSHHost))
				}
				return nil
			},
		})
	}

	p.Steps = append(p.Steps, plan.Step{
		ID: "import", Title: "Register the hub in " + req.Host + "'s lazylongarm config", Idempotent: true,
		Actions: []plan.Action{{Where: where, Display: f.Bin + " hub import --stdin --yes --json   (hub entry" + map[bool]string{true: " + targets", false: ""}[len(kept) > 0] + " over stdin)"}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			r := run(`"$1" hub import --stdin --yes --json`, f.Bin)
			r.Stdin = body
			res, err := env.RunQuiet(ctx, r)
			if err != nil {
				return err
			}
			var out struct {
				Merge hubshare.MergeResult `json:"merge"`
			}
			if err := json.Unmarshal(jsonPart(res.Stdout), &out); err == nil && out.Merge.HubID != "" {
				m := out.Merge
				env.Set("remote_hub_id", m.HubID)
				env.Logf("  hub %s %s there (default: %v); targets +%d ~%d", m.HubID, m.Hub, m.Default, len(m.TargetsAdded), len(m.TargetsUpdated))
			}
			return nil
		},
	})

	p.Steps = append(p.Steps, plan.Step{
		ID: "verify", Title: "Check the hub from " + req.Host, Idempotent: true, ReadOnly: true,
		Actions: []plan.Action{{Where: where, Display: f.Bin + " hub status --json   (hub service over ssh, its longarm enrollment)"}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			id := env.Get("remote_hub_id")
			if id == "" {
				id = h.ID
			}
			res, err := env.RunQuiet(ctx, run(`"$1" hub status --hub "$2" --json`, f.Bin, id))
			if err != nil {
				return err
			}
			var st struct {
				Checks []struct{ ID, Status, Detail string } `json:"checks"`
			}
			if err := json.Unmarshal(jsonPart(res.Stdout), &st); err != nil {
				return fmt.Errorf("hub status on %s: %w", req.Host, err)
			}
			var bad []string
			for _, c := range st.Checks {
				env.Logf("  %-9s %-4s %s", c.ID, c.Status, c.Detail)
				if (c.ID == "service" || c.ID == "client" || c.ID == "reachable") && c.Status != "ok" {
					bad = append(bad, c.ID+": "+c.Detail)
				}
			}
			if len(bad) > 0 {
				return plan.WithFix(fmt.Errorf("hub status on %s: %s", req.Host, strings.Join(bad, "; ")), "run `lazylongarm hub status` there")
			}
			return nil
		},
	})
	return p, nil
}

// jsonPart is out from its first '{' (a --json result after any log lines).
func jsonPart(out []byte) []byte {
	if i := bytes.IndexByte(out, '{'); i >= 0 {
		return out[i:]
	}
	return out
}
