package flows

import (
	"context"
	"fmt"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/hubhost"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
)

// Steps shared by `hub setup` and `hub panel-access`.

// Env keys the shared steps exchange.
const (
	envHubConfigChanged = "hub_config_changed" // hub-config rewrote hub.toml in this run
	envHubStarted       = "hub_started"        // the service step started the hub in this run
)

// remote returns a request builder for scripts on host ($1.. are args).
func remote(host string) func(script string, args ...string) connection.Request {
	return func(script string, args ...string) connection.Request {
		return connection.Request{Host: host, Script: script, Args: args, Timeout: 5 * time.Minute}
	}
}

// aptKeyScript downloads an apt signing key, refuses an unexpected
// fingerprint, installs key + sources and refreshes the index.
// Args: $1 key URL, $2 fingerprint, $3 keyring, $4 sources path, $5 sources content.
const aptKeyScript = `set -eu
install -d -m 0755 "$(dirname "$3")"
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

// aptRepoStep adds an apt repository whose signing key must match fpr.
func aptRepoStep(id, title, host, keyURL, fpr, keyring, sources, content string) plan.Step {
	run := remote(host)
	return plan.Step{
		ID: id, Title: title, Idempotent: true,
		Actions: []plan.Action{{Where: host, Privileged: true, Display: fmt.Sprintf("curl %s → verify fingerprint %s → %s\nwrite %s; apt-get update", keyURL, fpr, keyring, sources)}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			r := run(`test -s "$1" && test -s "$2"`, keyring, sources)
			r.Timeout = 30 * time.Second
			_, err := env.RunQuiet(ctx, r)
			return checkResult(err)
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			_, err := env.RunPrivileged(ctx, run(aptKeyScript, keyURL, fpr, keyring, sources, content))
			return err
		},
	}
}

// aptInstallStep installs a package unless dpkg already has it.
func aptInstallStep(id, title, host, pkg, display string) plan.Step {
	run := remote(host)
	return plan.Step{
		ID: id, Title: title, Idempotent: true,
		Actions: []plan.Action{{Where: host, Privileged: true, Display: display}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			r := run(`dpkg-query -W -f='${Status}' "$1" 2>/dev/null | grep -q 'install ok installed'`, pkg)
			r.Timeout = 30 * time.Second
			_, err := env.RunQuiet(ctx, r)
			return checkResult(err)
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			_, err := env.RunPrivileged(ctx, run(`DEBIAN_FRONTEND=noninteractive apt-get install -y -q "$1"`, pkg))
			return err
		},
	}
}

// hubConfigStep writes hub.toml (backing up the previous file) unless its
// sha256 already matches; a write marks the hub for restart-hub.
func hubConfigStep(host string, settings hubhost.Settings) plan.Step {
	run := remote(host)
	content := hubhost.Render(settings)
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
	return plan.Step{
		ID: "hub-config", Title: "Write " + hubhost.ConfigPath, Idempotent: true,
		Actions: []plan.Action{{Where: host, Privileged: true, Display: "write " + hubhost.ConfigPath + " (root:longarm-hub 0640, previous file backed up):\n" + indent(content, "  ")}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			r := run(`[ "$(sha256sum "$1" 2>/dev/null | cut -d' ' -f1)" = "$2" ]`, hubhost.ConfigPath, sum)
			r.Sudo, r.Timeout = true, 30*time.Second
			_, err := env.RunQuiet(ctx, r)
			return checkResult(err)
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			if _, err := env.RunPrivileged(ctx, run(writeScript, hubhost.ConfigPath)); err != nil {
				return err
			}
			env.Set(envHubConfigChanged, "1")
			return nil
		},
	}
}

// restartHubStep restarts a running hub when hub.toml (or its unit) changed
// in this run; a hub the service step just started is left alone.
func restartHubStep(host, panel string) plan.Step {
	run := remote(host)
	return plan.Step{
		ID: "restart-hub", Title: "Restart longarm-hub when its configuration changed", Idempotent: true,
		Actions: []plan.Action{{Where: host, Privileged: true, Display: "systemctl restart longarm-hub   (only when this run rewrote hub.toml or the unit); wait for http://" + panel + "/healthz"}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			return env.Get(envHubConfigChanged) != "1" || env.Get(envHubStarted) == "1", nil
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			_, err := env.RunPrivileged(ctx, run(`systemctl daemon-reload && systemctl restart longarm-hub 2>&1`))
			return err
		},
		Verify: func(ctx context.Context, env *plan.Env) error { return waitHealthy(ctx, env, host, panel) },
	}
}

// waitHealthy polls the hub's loopback /healthz for up to 30 s.
func waitHealthy(ctx context.Context, env *plan.Env, host, panel string) error {
	r := remote(host)(`i=0; while [ $i -lt 30 ]; do out=$(curl -fsS --max-time 2 "http://$1/healthz" 2>/dev/null) && { printf '%s\n' "$out"; exit 0; }; i=$((i+1)); sleep 1; done; systemctl status --no-pager -n 20 longarm-hub >&2 || true; exit 1`, panel)
	r.Timeout = 60 * time.Second
	res, err := env.RunQuiet(ctx, r)
	if err != nil {
		return plan.WithFix(fmt.Errorf("longarm-hub did not become healthy: %w", err), "ssh "+host+" sudo journalctl -u longarm-hub -n 50")
	}
	env.Logf("  healthz: %s", hubhost.HealthSummary(string(res.Stdout)))
	return nil
}

// withPanelAccess adds the recorded HTTPS panel settings to hub.toml
// settings, so rerunning hub setup keeps `hub panel-access public`.
func withPanelAccess(s hubhost.Settings, pa *config.PanelAccess) hubhost.Settings {
	if pa == nil || pa.Mode != config.PanelPublic || pa.Domain == "" {
		return s
	}
	s.PublicURL = "https://" + pa.Domain
	s.HTTPS = ":443"
	if pa.Redirect {
		s.HTTPSRedirect = ":80"
	}
	s.ACMEEmail = pa.ACMEEmail
	return s
}
