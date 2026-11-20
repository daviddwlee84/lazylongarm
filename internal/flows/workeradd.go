package flows

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/hostprobe"
	"github.com/daviddwlee84/lazylongarm/internal/longarm"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/release"
	"github.com/daviddwlee84/lazylongarm/internal/secret"
)

// Service modes for a worker.
const (
	ServiceSystem = "system" // `longarmd service install` (systemd / LaunchDaemon), needs sudo once
	ServiceUser   = "user"   // background `longarmd run` as the SSH user; no sudo, stops at reboot
)

// userDaemonPattern finds a background `longarmd run` with pgrep -f. It is
// anchored at the start of the command line (argv[0] ending in longarmd):
// the check itself runs as `sh -c '<script>'` under the login shell, whose
// command lines contain the words "longarmd run" too, so an unanchored
// pattern always matched its own parent shell and the step was skipped as
// "already in place" with no daemon running.
const userDaemonPattern = `^[^ ]*longarmd run( |$)`

// WorkerRequest describes `worker add`.
type WorkerRequest struct {
	Host          string   // SSH alias; "" = this machine
	Name          string   // worker name (default: host alias / hostname)
	Labels        []string // KEY=VALUE
	Service       string   // ServiceSystem | ServiceUser
	DirectTailnet bool     // also listen on the tailnet IP (+ hub relay) for direct paths
	Hub           *config.Hub
	Version       string // release tag for pushed binaries
	// Mint selects how the join code is created: MintAdmin (`longarm admin
	// join-code` + `admin approve`, a fleet worker; works for any admin) or
	// MintOwner (`longarm worker add`, a worker owned by the caller's Longarm
	// user; needs a user record on the hub).
	Mint string
	// Reenroll enrolls again although the machine already has a certificate
	// (after `longarm admin revoke`, a lost key or a new hub CA).
	Reenroll bool
}

// Join code sources.
const (
	MintAdmin = "admin"
	MintOwner = "owner"
)

// WorkerDeps are the side effects of a worker plan.
type WorkerDeps struct {
	Config   *config.Config
	Client   LongarmClient
	CacheDir string
	// Fetch returns a verified local copy of a release asset.
	Fetch func(ctx context.Context, tag, asset string, logf func(string, ...any)) (string, error)
	// Online waits for a worker to report online (injectable for tests).
	PollInterval time.Duration
	WaitTimeout  time.Duration
}

var workerNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// WorkerDefaults fills names from facts.
func WorkerDefaults(req WorkerRequest, f *hostprobe.Facts) WorkerRequest {
	if req.Name == "" {
		switch {
		case req.Host != "":
			req.Name = config.Slug(req.Host)
		case f != nil && f.Hostname != "":
			req.Name = config.Slug(strings.TrimSuffix(f.Hostname, ".local"))
		}
	}
	if req.Service == "" {
		req.Service = ServiceSystem
	}
	if req.Mint == "" {
		req.Mint = MintAdmin
	}
	return req
}

// ValidateWorker checks the request.
func ValidateWorker(req WorkerRequest, f *hostprobe.Facts) error {
	var problems []string
	if req.Hub == nil {
		problems = append(problems, "no hub configured: run `lazylongarm hub setup` (or join a hub) first")
	} else if req.Hub.CAFingerprint == "" {
		problems = append(problems, "the hub profile has no CA fingerprint")
	}
	if !workerNameRE.MatchString(req.Name) {
		problems = append(problems, fmt.Sprintf("worker name %q must be letters, digits, '.', '_' or '-'", req.Name))
	}
	for _, l := range req.Labels {
		if !strings.Contains(l, "=") {
			problems = append(problems, fmt.Sprintf("label %q must be KEY=VALUE", l))
		}
	}
	if req.Service != ServiceSystem && req.Service != ServiceUser {
		problems = append(problems, "service must be system or user")
	}
	if f != nil {
		if f.OS != "linux" && f.OS != "darwin" {
			problems = append(problems, "workers on "+f.OS+" are not supported by lazylongarm yet")
		}
		if f.Arch != "amd64" && f.Arch != "arm64" {
			problems = append(problems, "unsupported CPU architecture "+f.RawArch)
		}
		if req.Service == ServiceSystem && f.Sudo == "absent" {
			problems = append(problems, "sudo is not available on "+f.Where()+"; choose the user service mode")
		}
		if req.DirectTailnet && f.TailscaleIP == "" {
			problems = append(problems, "direct tailnet listening needs a Tailscale IP on "+f.Where())
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// BuildWorkerAdd assembles the worker add plan.
func BuildWorkerAdd(req WorkerRequest, f *hostprobe.Facts, deps WorkerDeps) (*plan.Plan, error) {
	req = WorkerDefaults(req, f)
	if err := ValidateWorker(req, f); err != nil {
		return nil, err
	}
	if deps.PollInterval == 0 {
		deps.PollInterval = 3 * time.Second
	}
	if deps.WaitTimeout == 0 {
		deps.WaitTimeout = 120 * time.Second
	}
	host := req.Host
	where := host
	if where == "" {
		where = "local"
	}
	hub := req.Hub
	binDir := filepath.Join(f.Home, ".local", "bin")
	longarmdPath := filepath.Join(binDir, "longarmd")
	longarmPath := filepath.Join(binDir, "longarm")
	tag := req.Version
	listen := []string{}
	if req.DirectTailnet {
		listen = []string{"tcp://127.0.0.1:7701", "tcp://" + f.TailscaleIP + ":7701", "longnet://" + hub.Address + "?name=" + req.Name}
	}
	svcDesc := map[string]string{
		ServiceSystem: map[string]string{"linux": "systemd unit (User=" + f.User + ")", "darwin": "LaunchDaemon com.longarm.longarmd (UserName=" + f.User + ")"}[f.OS],
		ServiceUser:   "background `longarmd run` as " + f.User + " (no sudo; restart it after a reboot)",
	}[req.Service]
	path := "via the hub relay (outbound only)"
	if req.DirectTailnet {
		path = "direct on " + f.TailscaleIP + ":7701 for tailnet peers, plus the hub relay"
	}
	p := &plan.Plan{
		Kind:  "worker-add",
		Key:   where + "--" + req.Name,
		Title: fmt.Sprintf("Add %s as Longarm worker %q", f.Where(), req.Name),
		Summary: [][2]string{
			{"machine", fmt.Sprintf("%s · %s · %s · %d CPU · %s", f.Where(), f.Platform(), f.Distro, f.CPUs, f.MemSummary())},
			{"worker name", req.Name},
			{"hub", hub.Address + " (" + hub.ID + ")"},
			{"binaries", fmt.Sprintf("longarmd + longarm %s → %s", orLatest(tag), binDir)},
			{"service", svcDesc},
			{"reachable", path},
			{"ownership", map[string]string{MintAdmin: "fleet worker (admin join code, approved by you right after it joins)", MintOwner: "your own worker: only you can run jobs on it until you share it"}[req.Mint]},
		},
	}
	if len(req.Labels) > 0 {
		p.Summary = append(p.Summary, [2]string{"labels", strings.Join(req.Labels, ", ")})
	}
	if req.Service == ServiceSystem && f.Sudo == "password" {
		p.Warnings = append(p.Warnings, "sudo on "+f.Where()+" asks for a password: the service step hands the terminal to sudo")
	}
	if f.OS == "darwin" {
		p.Warnings = append(p.Warnings, "macOS workers run jobs as "+f.User+" (no per-user isolation on macOS)")
	}
	if existing := f.Bins["longarmd"]; existing != "" && existing != longarmdPath {
		p.Warnings = append(p.Warnings, fmt.Sprintf("another longarmd is installed at %s (%s); the worker will use %s", existing, f.Versions["longarmd"], longarmdPath))
	}

	run := func(script string, args ...string) connection.Request {
		return connection.Request{Host: host, Script: script, Args: args, Timeout: 2 * time.Minute}
	}

	// 1. binaries
	type asset struct{ component, dest string }
	assets := []asset{{"longarmd", longarmdPath}, {"longarm", longarmPath}}
	resolveTag := func(ctx context.Context, env *plan.Env) (string, error) {
		if t := env.Get("release_tag"); t != "" {
			return t, nil
		}
		t := tag
		if t == "" {
			var err error
			if t, err = release.Latest(ctx); err != nil {
				return "", err
			}
		}
		env.Set("release_tag", t)
		return t, nil
	}
	p.Steps = append(p.Steps, plan.Step{
		ID: "binaries", Title: "Install longarmd + longarm into " + binDir, Idempotent: true,
		Actions: []plan.Action{
			{Where: "local", Display: fmt.Sprintf("download %s and %s (%s) from GitHub, verify SHA256SUMS (cached)", release.AssetName("longarmd", f.OS, f.Arch), release.AssetName("longarm", f.OS, f.Arch), orLatest(tag))},
			{Where: where, Display: "copy to " + longarmdPath + " and " + longarmPath + " (0755, via ssh stdin, atomic rename)"},
		},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			t, err := resolveTag(ctx, env)
			if err != nil {
				return false, err
			}
			for _, a := range assets {
				local, err := deps.Fetch(ctx, t, release.AssetName(a.component, f.OS, f.Arch), env.Logf)
				if err != nil {
					return false, err
				}
				want, err := release.SHA256(local)
				if err != nil {
					return false, err
				}
				r := run(`if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi 2>/dev/null | cut -d' ' -f1`, a.dest)
				r.Timeout = 30 * time.Second
				res, err := env.RunQuiet(ctx, r)
				if err != nil {
					var ce *connection.CommandError
					if errors.As(err, &ce) {
						return false, nil
					}
					return false, err
				}
				if strings.TrimSpace(string(res.Stdout)) != want {
					return false, nil
				}
			}
			env.Logf("  %s already has longarmd/longarm %s", f.Where(), env.Get("release_tag"))
			return true, nil
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			t, err := resolveTag(ctx, env)
			if err != nil {
				return err
			}
			for _, a := range assets {
				local, err := deps.Fetch(ctx, t, release.AssetName(a.component, f.OS, f.Arch), env.Logf)
				if err != nil {
					return err
				}
				data, err := os.ReadFile(local)
				if err != nil {
					return err
				}
				if host == "" && samePath(local, a.dest) {
					continue
				}
				env.Logf("  copying %s (%.1f MB) → %s:%s", filepath.Base(local), float64(len(data))/1e6, where, a.dest)
				if err := connection.PushFile(ctx, host, data, a.dest, "0755"); err != nil {
					return err
				}
			}
			return nil
		},
		Verify: func(ctx context.Context, env *plan.Env) error {
			res, err := env.RunQuiet(ctx, run(`"$1" --version 2>/dev/null || "$1" version`, longarmdPath))
			if err != nil {
				return fmt.Errorf("installed longarmd does not run on %s: %w", f.Where(), err)
			}
			env.Logf("  longarmd %s", hostprobe.ExtractVersion(strings.TrimSpace(string(res.Stdout))))
			return nil
		},
	})

	// 2. enroll
	enrolled := func(ctx context.Context, env *plan.Env) (bool, error) {
		r := run(`test -d "$HOME/.longarm/tls" && grep -qF "$1" "$HOME/.longarm/hub.toml" 2>/dev/null`, strings.Split(hub.Address, ":")[0])
		r.Timeout = 30 * time.Second
		_, err := env.RunQuiet(ctx, r)
		return checkResult(err)
	}
	p.Steps = append(p.Steps, plan.Step{
		ID: "enroll", Title: "Enroll the worker with the hub (one-time code via stdin)", Idempotent: false,
		Actions: enrollActions(req, where, hub),
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			if req.Reenroll {
				return false, nil
			}
			ok, err := enrolled(ctx, env)
			if ok {
				env.Logf("  %s is already enrolled with %s", f.Where(), hub.Address)
			}
			return ok, err
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			var code secret.Secret
			var err error
			if req.Mint == MintOwner {
				code, err = deps.Client.WorkerAdd(ctx, req.Name, 15*time.Minute, req.Labels)
			} else {
				code, err = deps.Client.AdminJoinCode(ctx, longarm.JoinCodeRequest{Role: "worker", Name: req.Name, TTL: 15 * time.Minute, Labels: req.Labels})
			}
			if err != nil {
				return err
			}
			env.SetSecret("worker_code", code)
			env.Logf("  minted a one-time worker code (valid 15 min)")
			r := run(`set -eu
IFS= read -r code
if [ "$5" = force ]; then exec "$1" join "$2" "$code" --ca-fingerprint "$3" --name "$4" --no-wait --force; fi
exec "$1" join "$2" "$code" --ca-fingerprint "$3" --name "$4" --no-wait`, longarmdPath, hub.Address, hub.CAFingerprint, req.Name, map[bool]string{true: "force", false: "-"}[req.Reenroll])
			r.Stdin = []byte(code.Reveal() + "\n")
			r.Timeout = 90 * time.Second
			if _, err := env.Run(ctx, r); err != nil {
				return &plan.UnknownError{Err: err}
			}
			if req.Mint == MintAdmin && !hubAutoApproves(hub) {
				return approve(ctx, env, deps, req.Name)
			}
			return nil
		},
	})

	// 3. service
	var installArgs []string
	for _, l := range listen {
		installArgs = append(installArgs, "--listen", l)
	}
	switch req.Service {
	case ServiceSystem:
		display := fmt.Sprintf("%s service install --user %s%s\n%s service start", longarmdPath, f.User, joinArgs(installArgs), longarmdPath)
		p.Steps = append(p.Steps, plan.Step{
			ID: "service", Title: "Install and start the longarmd service", Idempotent: true,
			Actions: []plan.Action{{Where: where, Privileged: true, Display: display}},
			Check: func(ctx context.Context, env *plan.Env) (bool, error) {
				r := run(`if command -v systemctl >/dev/null 2>&1; then systemctl is-active --quiet longarmd || systemctl is-active --quiet "longarmd@$(id -un)"; else launchctl print system/com.longarm.longarmd >/dev/null 2>&1; fi`)
				r.Timeout = 30 * time.Second
				_, err := env.RunQuiet(ctx, r)
				return checkResult(err)
			},
			Apply: func(ctx context.Context, env *plan.Env) error {
				args := append([]string{longarmdPath, f.User}, installArgs...)
				_, err := env.RunPrivileged(ctx, run(`set -eu
bin="$1"; user="$2"; shift 2
"$bin" service install --user "$user" "$@"
"$bin" service start`, args...))
				return err
			},
		})
	case ServiceUser:
		p.Steps = append(p.Steps, plan.Step{
			ID: "service", Title: "Start longarmd in the background as " + f.User, Idempotent: true,
			Actions: []plan.Action{{Where: where, Display: fmt.Sprintf("nohup %s run%s >> ~/.longarm/longarmd.out &", longarmdPath, joinArgs(installArgs))}},
			Check: func(ctx context.Context, env *plan.Env) (bool, error) {
				r := run(`pgrep -u "$(id -u)" -f '` + userDaemonPattern + `' >/dev/null 2>&1`)
				r.Timeout = 30 * time.Second
				_, err := env.RunQuiet(ctx, r)
				return checkResult(err)
			},
			Apply: func(ctx context.Context, env *plan.Env) error {
				args := append([]string{longarmdPath}, installArgs...)
				_, err := env.Run(ctx, run(`set -eu
bin="$1"; shift
mkdir -p "$HOME/.longarm"
if command -v setsid >/dev/null 2>&1; then
  setsid nohup "$bin" run "$@" >>"$HOME/.longarm/longarmd.out" 2>&1 </dev/null &
else
  nohup "$bin" run "$@" >>"$HOME/.longarm/longarmd.out" 2>&1 </dev/null &
fi
sleep 1
echo "longarmd started (pid $!)"`, args...))
				return err
			},
		})
	}

	// 4. wait online
	p.Steps = append(p.Steps, plan.Step{
		ID: "online", Title: "Wait until the hub reports the worker online", Idempotent: true,
		Actions: []plan.Action{{Where: "local", Display: fmt.Sprintf("poll `longarm worker ls --json` for %s (up to %s)", req.Name, deps.WaitTimeout)}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			deadline := time.Now().Add(deps.WaitTimeout)
			last := ""
			approved := false
			for {
				list, err := deps.Client.Workers(ctx)
				if err == nil {
					if n, ok := findNode(list.Nodes, req.Name); ok {
						state := n.State
						if n.State == "approved" {
							state = map[bool]string{true: "online", false: "offline"}[n.Online]
						}
						if state != last {
							env.Logf("  %s: %s", req.Name, state)
							last = state
						}
						if n.Online {
							env.Set("worker_path", n.Path())
							return nil
						}
						if n.State == "pending" {
							if req.Mint == MintAdmin && !approved {
								approved = true
								if err := deps.Client.AdminApprove(ctx, req.Name); err != nil {
									env.Logf("  ! longarm admin approve %s: %v", req.Name, err)
								} else {
									env.Logf("  approved %s", req.Name)
								}
							} else if time.Now().After(deadline.Add(-deps.WaitTimeout / 2)) {
								return plan.WithFix(errors.New("the worker is waiting for approval"), "longarm admin approve "+req.Name)
							}
						}
					}
				}
				if time.Now().After(deadline) {
					return plan.WithFix(fmt.Errorf("%s did not come online within %s", req.Name, deps.WaitTimeout), serviceLogHint(host, f))
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(deps.PollInterval):
				}
			}
		},
	})

	// 5. try it
	p.Steps = append(p.Steps, plan.Step{
		ID: "try-it", Title: "Try it: run a command on the new worker", Idempotent: true, ReadOnly: true,
		Actions: []plan.Action{{Where: "local", Display: "longarm exec " + req.Name + " -- uname -a"}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			out, err := deps.Client.Exec(ctx, req.Name, "uname", "-a")
			if err != nil {
				return err
			}
			env.Logf("  │ %s", strings.TrimSpace(string(out)))
			return nil
		},
	})

	// 6. record
	p.Steps = append(p.Steps, plan.Step{
		ID: "save-target", Title: "Record the worker in lazylongarm's config", Idempotent: true,
		Actions: []plan.Action{{Where: "local", Display: "save target (ssh host, worker name, install + service mode)"}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			if deps.Config == nil {
				return nil
			}
			t := workerTarget(req, f)
			// Re-read under the lock: another `worker add` may have saved
			// its own target while this one was busy for minutes.
			if err := deps.Config.Update(func(c *config.Config) error {
				c.UpsertTarget(t)
				return nil
			}); err != nil {
				return err
			}
			env.Logf("  saved target %q", t.ID)
			return nil
		},
	})
	return p, nil
}

func enrollActions(req WorkerRequest, where string, hub *config.Hub) []plan.Action {
	force := ""
	if req.Reenroll {
		force = " --force"
	}
	join := plan.Action{Where: where, Display: fmt.Sprintf("longarmd join %s <code from stdin> --ca-fingerprint %s --name %s --no-wait%s", hub.Address, shortFP(hub.CAFingerprint), req.Name, force)}
	if req.Mint == MintOwner {
		return []plan.Action{
			{Where: "local", Display: fmt.Sprintf("longarm worker add %s --ttl 15m%s   → one-time code (pre-approved, owned by you)", req.Name, labelArgs(req.Labels))},
			join,
		}
	}
	labels := ""
	if len(req.Labels) > 0 {
		labels = " --labels " + strings.Join(req.Labels, ",")
	}
	return []plan.Action{
		{Where: "local", Display: fmt.Sprintf("longarm admin join-code --role worker --name %s --ttl 15m%s   → one-time code", req.Name, labels)},
		join,
		{Where: "local", Display: "longarm admin approve " + req.Name},
	}
}

// approve waits for the joining node to be registered, then approves it.
func approve(ctx context.Context, env *plan.Env, deps WorkerDeps, name string) error {
	var err error
	for i := 0; i < 10; i++ {
		if err = deps.Client.AdminApprove(ctx, name); err == nil {
			env.Logf("  approved %s", name)
			return nil
		}
		if longarm.KindOf(err) != longarm.KindNotFound {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(deps.PollInterval):
		}
	}
	// Not fatal: the online step approves a pending node again.
	env.Logf("  ! longarm admin approve %s: %v (will retry while waiting for it)", name, err)
	return nil
}

// hubAutoApproves is unknown for hubs we did not set up; assume approval is needed.
func hubAutoApproves(*config.Hub) bool { return false }

func findNode(nodes []longarm.Node, name string) (longarm.Node, bool) {
	for _, n := range nodes {
		if n.Name == name && n.IsWorker() {
			return n, true
		}
	}
	return longarm.Node{}, false
}

func serviceLogHint(host string, f *hostprobe.Facts) string {
	cmd := "journalctl -u longarmd -n 50"
	if f.OS == "darwin" {
		cmd = "tail -n 50 ~/.longarm/longarmd.log"
	}
	if host == "" {
		return cmd
	}
	return "ssh " + host + " " + cmd
}

func labelArgs(labels []string) string {
	var b strings.Builder
	for _, l := range labels {
		b.WriteString(" --label " + l)
	}
	return b.String()
}

func joinArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return " " + strings.Join(args, " ")
}

func shortFP(fp string) string {
	if len(fp) > 24 {
		return fp[:24] + "…"
	}
	return fp
}

func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}
