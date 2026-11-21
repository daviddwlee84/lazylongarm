package flows

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/hostprobe"
	"github.com/daviddwlee84/lazylongarm/internal/longarm"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
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
	Version       string // release tag of the installed binaries
	// Fetch is how the binaries reach the machine: FetchAuto (default),
	// FetchPush or FetchRemote. This machine (Host == "") always installs
	// from the local verified cache.
	Fetch string
	// Mint selects how the join code is created: MintAdmin (`longarm admin
	// join-code` + `admin approve`, a fleet worker; works for any admin) or
	// MintOwner (`longarm worker add`, a worker owned by the caller's Longarm
	// user; needs a user record on the hub).
	Mint string
	// Reenroll enrolls again although the machine already has a certificate
	// (after `longarm admin revoke`, a lost key or a new hub CA), and
	// restarts the service so longarmd picks up the new identity (it only
	// reloads certificates of the same node).
	Reenroll bool
	// Owner makes it a worker owned by this Longarm user: an admin join code
	// with --owner, which joins approved and is usable by that user only.
	// Needs an admin certificate (MintAdmin); the caller should act as the
	// same user, or the new worker is invisible to it.
	Owner string
	// Replace is the hub's current node of this name, revoked right before
	// the machine enrolls again (with Reenroll): node names are unique, and
	// a fleet worker cannot become owned in place. It gets a new node ID.
	Replace *longarm.Node
}

// userNameRE is upstream's rule for Longarm user names (internal/pki).
var userNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,16}$`)

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
	// Checksum returns an asset's sha256 from the release's SHA256SUMS
	// without downloading the asset (nil: Fetch it and hash the copy).
	Checksum func(ctx context.Context, tag, asset string) (string, error)
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
	if req.Fetch == "" {
		req.Fetch = FetchAuto
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
	if !validFetch(req.Fetch) {
		problems = append(problems, fmt.Sprintf("fetch %q must be auto, push or remote", req.Fetch))
	}
	if req.Owner != "" {
		if !userNameRE.MatchString(req.Owner) {
			problems = append(problems, fmt.Sprintf("owner %q is not a Longarm user name (lowercase letter first, then a-z 0-9 _ -, at most 17)", req.Owner))
		}
		if req.Mint != MintAdmin {
			problems = append(problems, "--owner mints an admin join code, which needs an admin certificate (or drop --owner: `longarm worker add` makes a worker owned by you)")
		}
	}
	if req.Replace != nil && !req.Reenroll {
		problems = append(problems, fmt.Sprintf("worker %s is already on the hub: pass --reenroll to replace it", req.Name))
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
			{"fetch", fetchSummary(req.Fetch, host)},
			{"service", svcDesc},
			{"reachable", path},
			{"ownership", ownership(req)},
		},
	}
	if len(req.Labels) > 0 {
		p.Summary = append(p.Summary, [2]string{"labels", strings.Join(req.Labels, ", ")})
	}
	if r := req.Replace; r != nil {
		p.Summary = append(p.Summary, [2]string{"replaces", fmt.Sprintf("node %s (id %s, %s): revoked right before the machine enrolls again", r.Name, shortID(r.ID), ownerText(r.Owner))})
		p.Warnings = append(p.Warnings, fmt.Sprintf("%s gets a new node ID and is offline between the revoke and the service restart; its port links are removed, and its job history stays with the old ID", req.Name))
	}
	if req.Reenroll {
		p.Warnings = append(p.Warnings, "the service restarts to pick up the new certificate: jobs keep running, sessions and forwards through it reconnect")
	}
	if req.Service == ServiceSystem && f.Sudo == "password" {
		if req.Reenroll {
			p.Warnings = append(p.Warnings, "sudo on "+f.Where()+" asks for a password: needed only if longarmd does not come back by itself (then the service step hands the terminal to sudo)")
		} else {
			p.Warnings = append(p.Warnings, "sudo on "+f.Where()+" asks for a password: the service step hands the terminal to sudo")
		}
	}
	if f.OS == "darwin" {
		p.Warnings = append(p.Warnings, "macOS workers run jobs as "+f.User+" (no per-user isolation on macOS)")
	}
	if w := f.ClockWarning(); w != "" {
		p.Warnings = append(p.Warnings, w)
	}
	if existing := f.Bins["longarmd"]; existing != "" && existing != longarmdPath {
		p.Warnings = append(p.Warnings, fmt.Sprintf("another longarmd is installed at %s (%s); the worker will use %s", existing, f.Versions["longarmd"], longarmdPath))
	}

	run := func(script string, args ...string) connection.Request {
		return connection.Request{Host: host, Script: script, Args: args, Timeout: 2 * time.Minute}
	}

	// 1. binaries
	p.Steps = append(p.Steps, binariesStep(host, where, f, tag, effectiveFetch(req.Fetch, host), binDir,
		[]binary{{"longarmd", longarmdPath}, {"longarm", longarmPath}}, deps))

	// 2. enroll
	// enrollState is "enrolled" (a certificate is there), "requested" (joined
	// this hub, but not approved yet or longarmd has not collected the
	// certificate) or "" (not joined to this hub).
	enrollState := func(ctx context.Context, env *plan.Env) (string, error) {
		r := run(`grep -qF "$1" "$HOME/.longarm/hub.toml" 2>/dev/null || exit 0
t="$HOME/.longarm/tls"
if [ -f "$t/node.crt" ]; then echo enrolled
elif [ -f "$t/pending.key" ]; then echo requested
elif [ -d "$t" ]; then echo enrolled
fi`, strings.Split(hub.Address, ":")[0])
		r.Timeout = 30 * time.Second
		res, err := env.RunQuiet(ctx, r)
		if ok, err := checkResult(err); !ok {
			return "", err
		}
		return strings.TrimSpace(string(res.Stdout)), nil
	}
	p.Steps = append(p.Steps, plan.Step{
		ID: "enroll", Title: "Enroll the worker with the hub (one-time code via stdin)", Idempotent: false,
		Actions: enrollActions(req, where, hub),
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			if req.Reenroll {
				return false, nil
			}
			state, err := enrollState(ctx, env)
			switch state {
			case "enrolled":
				env.Logf("  %s is already enrolled with %s", f.Where(), hub.Address)
			case "requested":
				// Joining again would leave a second pending node behind.
				env.Logf("  %s already asked to join %s; it has no certificate yet (longarmd collects it once the hub approves it and can reach the hub)", f.Where(), hub.Address)
			}
			return state != "", err
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			var code secret.Secret
			var err error
			if req.Mint == MintOwner {
				code, err = deps.Client.WorkerAdd(ctx, req.Name, 15*time.Minute, req.Labels)
			} else {
				code, err = deps.Client.AdminJoinCode(ctx, longarm.JoinCodeRequest{Role: "worker", Name: req.Name, Owner: req.Owner, TTL: 15 * time.Minute, Labels: req.Labels})
			}
			if err != nil {
				return err
			}
			env.SetSecret("worker_code", code)
			env.Logf("  minted a one-time worker code (valid 15 min)")
			// Revoke only once the code exists: a failed mint leaves the
			// old node working. A rerun finds it gone (not found is fine).
			if req.Replace != nil {
				if err := deps.Client.AdminRevoke(ctx, req.Name); err != nil && longarm.KindOf(err) != longarm.KindNotFound {
					return err
				}
				env.Logf("  revoked the old node %s (id %s)", req.Name, shortID(req.Replace.ID))
			}
			r := run(`set -eu
IFS= read -r code
if [ "$5" = force ]; then exec "$1" join "$2" "$code" --ca-fingerprint "$3" --name "$4" --no-wait --force; fi
exec "$1" join "$2" "$code" --ca-fingerprint "$3" --name "$4" --no-wait`, longarmdPath, hub.Address, hub.CAFingerprint, req.Name, map[bool]string{true: "force", false: "-"}[req.Reenroll])
			r.Stdin = []byte(code.Reveal() + "\n")
			r.Timeout = 90 * time.Second
			if _, err := env.Run(ctx, r); err != nil {
				return &plan.UnknownError{Err: err}
			}
			if needsApproval(req) && !hubAutoApproves(hub) {
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
	restart := map[bool]string{true: "restart", false: "-"}[req.Reenroll]
	switch req.Service {
	case ServiceSystem:
		display := fmt.Sprintf("%s service install --user %s%s\n%s service start", longarmdPath, f.User, joinArgs(installArgs), longarmdPath)
		if req.Reenroll {
			display = fmt.Sprintf("end the running longarmd (as %s, no sudo): systemd/launchd start it again with the new certificate;\nif that does not work: %s service stop + start (sudo); not running: %s", f.User, longarmdPath, display)
		}
		p.Steps = append(p.Steps, plan.Step{
			ID: "service", Title: map[bool]string{true: "Restart the longarmd service with the new certificate", false: "Install and start the longarmd service"}[req.Reenroll], Idempotent: true,
			Actions: []plan.Action{{Where: where, Privileged: true, Display: display}},
			Check: func(ctx context.Context, env *plan.Env) (bool, error) {
				if req.Reenroll {
					return false, nil
				}
				r := run(`if command -v systemctl >/dev/null 2>&1; then systemctl is-active --quiet longarmd || systemctl is-active --quiet "longarmd@$(id -un)"; else launchctl print system/com.longarm.longarmd >/dev/null 2>&1; fi`)
				r.Timeout = 30 * time.Second
				_, err := env.RunQuiet(ctx, r)
				return checkResult(err)
			},
			Apply: func(ctx context.Context, env *plan.Env) error {
				if req.Reenroll {
					// The service runs longarmd as the SSH user, and both
					// unit (Restart=always, KillMode=process: jobs survive)
					// and LaunchDaemon (KeepAlive) start it again, so ending
					// it needs no sudo.
					r := run(`pat='` + userDaemonPattern + `'
old=$(pgrep -u "$(id -u)" -f "$pat" | head -n 1)
[ -n "$old" ] || exit 3
kill "$old"
i=0
while [ "$i" -lt 30 ]; do
  sleep 1
  new=$(pgrep -u "$(id -u)" -f "$pat" | head -n 1)
  if [ -n "$new" ] && [ "$new" != "$old" ]; then echo "longarmd restarted by the service manager (pid $old -> $new)"; exit 0; fi
  i=$((i+1))
done
exit 4`)
					r.Timeout = 60 * time.Second
					if _, err := env.Run(ctx, r); err == nil {
						return nil
					}
					env.Logf("  longarmd did not come back by itself; stopping and starting the service (sudo)")
				}
				args := append([]string{longarmdPath, f.User, restart}, installArgs...)
				_, err := env.RunPrivileged(ctx, run(`set -eu
bin="$1"; user="$2"; restart="$3"; shift 3
running() {
  if command -v systemctl >/dev/null 2>&1; then systemctl is-active --quiet longarmd || systemctl is-active --quiet "longarmd@$user"
  else launchctl print system/com.longarm.longarmd >/dev/null 2>&1; fi
}
if [ "$restart" = restart ] && running; then
  "$bin" service stop || true
  "$bin" service start
  exit 0
fi
"$bin" service install --user "$user" "$@"
"$bin" service start`, args...))
				return err
			},
		})
	case ServiceUser:
		p.Steps = append(p.Steps, plan.Step{
			ID: "service", Title: "Start longarmd in the background as " + f.User, Idempotent: true,
			Actions: []plan.Action{{Where: where, Display: fmt.Sprintf("%snohup %s run%s >> ~/.longarm/longarmd.out &", map[bool]string{true: "stop the running longarmd (new certificate), then ", false: ""}[req.Reenroll], longarmdPath, joinArgs(installArgs))}},
			Check: func(ctx context.Context, env *plan.Env) (bool, error) {
				if req.Reenroll {
					return false, nil
				}
				r := run(`pgrep -u "$(id -u)" -f '` + userDaemonPattern + `' >/dev/null 2>&1`)
				r.Timeout = 30 * time.Second
				_, err := env.RunQuiet(ctx, r)
				return checkResult(err)
			},
			Apply: func(ctx context.Context, env *plan.Env) error {
				args := append([]string{longarmdPath, restart}, installArgs...)
				_, err := env.Run(ctx, run(`set -eu
bin="$1"; restart="$2"; shift 2
if [ "$restart" = restart ]; then
  pkill -u "$(id -u)" -f '`+userDaemonPattern+`' 2>/dev/null || true
  sleep 1
fi
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
			hubApproved := false
			for {
				list, err := deps.Client.Workers(ctx)
				if err == nil {
					if n, ok := findNode(list.Nodes, req.Name, replacedID(req)); ok {
						state := n.State
						if n.State == "approved" {
							hubApproved = true
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
							if needsApproval(req) && !approved {
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
					err := fmt.Errorf("%s did not come online within %s%s", req.Name, deps.WaitTimeout, serviceLogs(host, f, req.Service))
					if hubApproved {
						if st, _ := enrollState(ctx, env); st == "requested" {
							err = fmt.Errorf("the hub approved %s, but %s never collected its certificate: longarmd there cannot reach %s (a proxy or firewall in the way, or a clock that is far off)", req.Name, where, hub.Address)
						}
					}
					return plan.WithFix(err, doctorHint(host, longarmdPath))
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
	var revoke []plan.Action
	if r := req.Replace; r != nil {
		revoke = []plan.Action{{Where: "local", Display: fmt.Sprintf("longarm admin revoke %s   (old node id %s, %s)", req.Name, shortID(r.ID), ownerText(r.Owner))}}
	}
	if req.Mint == MintOwner {
		return append(append([]plan.Action{
			{Where: "local", Display: fmt.Sprintf("longarm worker add %s --ttl 15m%s   → one-time code (pre-approved, owned by you)", req.Name, labelArgs(req.Labels))},
		}, revoke...), join)
	}
	labels := ""
	if len(req.Labels) > 0 {
		labels = " --labels " + strings.Join(req.Labels, ",")
	}
	if req.Owner != "" {
		return append(append([]plan.Action{
			{Where: "local", Display: fmt.Sprintf("longarm admin join-code --role worker --owner %s --name %s --ttl 15m%s   → one-time code (pre-approved)", req.Owner, req.Name, labels)},
		}, revoke...), join)
	}
	return append(append([]plan.Action{
		{Where: "local", Display: fmt.Sprintf("longarm admin join-code --role worker --name %s --ttl 15m%s   → one-time code", req.Name, labels)},
	}, revoke...), join, plan.Action{Where: "local", Display: "longarm admin approve " + req.Name})
}

func replacedID(req WorkerRequest) string {
	if req.Replace == nil {
		return ""
	}
	return req.Replace.ID
}

// needsApproval: a fleet worker joins pending; owned ones join approved.
func needsApproval(req WorkerRequest) bool { return req.Mint == MintAdmin && req.Owner == "" }

func ownership(req WorkerRequest) string {
	switch {
	case req.Owner != "":
		return fmt.Sprintf("owned by %s: only %s (and whom %s shares it with) can use it; it joins approved", req.Owner, req.Owner, req.Owner)
	case req.Mint == MintOwner:
		return "your own worker: only you can run jobs on it until you share it"
	default:
		return "fleet worker (admin join code, approved by you right after it joins): every user of the hub may use it"
	}
}

func ownerText(owner string) string {
	if owner == "" {
		return "fleet worker"
	}
	return "owned by " + owner
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
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

// findNode returns the live worker called name, skipping the node it
// replaces (skipID) while the hub still lists that one.
func findNode(nodes []longarm.Node, name, skipID string) (longarm.Node, bool) {
	for _, n := range nodes {
		if n.Name == name && n.IsWorker() && n.State != "revoked" && (skipID == "" || n.ID != skipID) {
			return n, true
		}
	}
	return longarm.Node{}, false
}

// doctorHint runs longarmd's own checklist (enrolled, certificate, running,
// hub link) on the machine.
func doctorHint(host, longarmdPath string) string {
	if host == "" {
		return longarmdPath + " doctor"
	}
	return "ssh " + host + " " + longarmdPath + " doctor"
}

// serviceLogs says where longarmd's output goes, as a suffix for an error.
// A launchd service on macOS keeps no log file.
func serviceLogs(host string, f *hostprobe.Facts, service string) string {
	var cmd string
	switch {
	case service == ServiceUser:
		cmd = "tail -n 50 ~/.longarm/longarmd.out"
	case f.OS == "linux":
		cmd = "journalctl -u longarmd -n 50"
	default:
		return ""
	}
	if host != "" {
		cmd = "ssh " + host + " " + cmd
	}
	return " (logs: " + cmd + ")"
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
