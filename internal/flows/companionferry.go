package flows

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/companion"
	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/hostprobe"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
)

// FerryDeps: the config to record results in, and the state reader (a test
// seam; nil runs companion.FerryStateScript on the host).
type FerryDeps struct {
	Config *config.Config
	State  func(ctx context.Context, env *plan.Env, host string) (companion.FerryState, error)
}

func (d *FerryDeps) state() func(context.Context, *plan.Env, string) (companion.FerryState, error) {
	if d.State == nil {
		return ReadFerryState
	}
	return d.State
}

// ReadFerryState runs companion.FerryStateScript on host.
func ReadFerryState(ctx context.Context, env *plan.Env, host string) (companion.FerryState, error) {
	res, err := env.RunQuiet(ctx, connection.Request{Host: host, Script: companion.FerryStateScript, Timeout: 30 * time.Second})
	if err != nil {
		return companion.FerryState{}, err
	}
	return companion.ParseFerryState(string(res.Stdout))
}

func where(host string) string {
	if host == "" {
		return "local"
	}
	return host
}

// OnHost names where to run command by hand: "on <host>: <command>" (a
// plain ssh one-liner could miss ~/.local/bin on PATH).
func OnHost(host, command string) string {
	if host == "" {
		return command
	}
	return "on " + host + ": " + command
}

// ---- companion ferry setup ----

// FerrySetupRequest asks to install ferry and backend CLIs on Host and
// sign the backends in.
type FerrySetupRequest struct {
	Host     string // SSH alias, "" = this machine
	Name     string // display name
	Spec     string // uv tool install source; "" = companion.DefaultFerrySpec
	Backends []string
	Upgrade  bool // reinstall ferry although it is installed
	// Login adds a step per backend that hands the terminal to the
	// backend's own login when it is not signed in. It needs Interactive.
	Login       bool
	Interactive bool
}

// BuildCompanionFerrySetup plans install ferry → install backends → logins
// → verify → record. current is the state read while planning; steps re-read
// it when applying.
func BuildCompanionFerrySetup(req FerrySetupRequest, f *hostprobe.Facts, current companion.FerryState, deps FerryDeps) (*plan.Plan, error) {
	read := deps.state()
	if req.Spec == "" {
		req.Spec = companion.DefaultFerrySpec
	}
	if len(req.Backends) == 0 {
		req.Backends = []string{companion.BackendBaiduPCS}
	}
	if req.Name == "" {
		req.Name = dash(req.Host)
	}
	at := where(req.Host)
	var problems []string
	if f.OS != "linux" && f.OS != "darwin" {
		problems = append(problems, fmt.Sprintf("%s runs %s; ferry companion setup supports Linux and macOS", f.Where(), dash(f.OS)))
	}
	if current.Ferry == "" && current.UV == "" && !f.Has["curl"] && !f.Has["wget"] {
		problems = append(problems, f.Where()+" has neither curl nor wget to install uv")
	}
	for _, b := range req.Backends {
		if err := companion.ValidBackend(b); err != nil {
			problems = append(problems, err.Error())
		}
	}
	baiduAsset := companion.BaiduPCSAsset(companion.BaiduPCSVersion, f.OS, f.Arch)
	if slices.Contains(req.Backends, companion.BackendBaiduPCS) && !current.Installed(companion.BackendBaiduPCS) && baiduAsset == "" {
		problems = append(problems, fmt.Sprintf("no BaiduPCS-Go release for %s/%s", dash(f.OS), dash(f.Arch)))
	}
	if req.Login && !req.Interactive {
		problems = append(problems, "backend logins need a terminal: run it in a terminal, or pass --no-login and sign in by hand")
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}

	ferryState := "not installed"
	if current.Ferry != "" {
		ferryState = "installed " + dash(current.Version)
	}
	var backendStates []string
	for _, b := range req.Backends {
		backendStates = append(backendStates, b+": "+current.BackendState(b))
	}
	p := &plan.Plan{
		Kind:  "companion-ferry-setup",
		Key:   at,
		Title: "ferry on " + f.Where(),
		Summary: [][2]string{
			{"machine", fmt.Sprintf("%s · %s · %s", f.Where(), f.Platform(), f.Distro)},
			{"ferry", ferryState},
			{"source", req.Spec},
			{"backends", strings.Join(backendStates, ", ")},
		},
	}

	outdated := current.Ferry != "" && !companion.VersionAtLeast(current.Version, companion.MinFerryVersion)
	upgrade := current.Ferry != "" && (req.Upgrade || outdated)
	title := "Install ferry"
	switch {
	case outdated:
		title = fmt.Sprintf("Upgrade ferry (%s is older than %s)", dash(current.Version), companion.MinFerryVersion)
	case upgrade:
		title = "Reinstall ferry"
	}
	mode := "install"
	if req.Upgrade || outdated {
		mode = "upgrade"
	}
	p.Steps = append(p.Steps, plan.Step{
		ID: "install-ferry", Title: title, Idempotent: true,
		Actions: []plan.Action{{Where: at, Display: companion.FerryInstallDisplay(req.Spec, upgrade, current.UV != "")}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			s, err := read(ctx, env, req.Host)
			return err == nil && s.Ferry != "" && companion.VersionAtLeast(s.Version, companion.MinFerryVersion) && !req.Upgrade, err
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			_, err := env.Run(ctx, connection.Request{Host: req.Host, Script: companion.FerryInstallScript, Args: []string{req.Spec, mode}, Timeout: 30 * time.Minute, Stall: 10 * time.Minute})
			return err
		},
	})
	if current.Ferry != "" && !upgrade {
		p.Warnings = append(p.Warnings, fmt.Sprintf("ferry %s is already installed on %s; pass --upgrade to reinstall it from %s (sender and receiver need the same ferry)", dash(current.Version), f.Where(), req.Spec))
	}

	for _, b := range req.Backends {
		installable := current.Installed(b)
		if b == companion.BackendBaiduPCS && !current.Installed(b) {
			installable = true
			url := companion.BaiduPCSURL(companion.BaiduPCSVersion, baiduAsset)
			p.Steps = append(p.Steps, plan.Step{
				ID: "install-baidupcs", Title: "Install BaiduPCS-Go " + companion.BaiduPCSVersion, Idempotent: true,
				Actions: []plan.Action{{Where: at, Display: "download " + url + "\ninstall the binary as ~/.local/bin/BaiduPCS-Go"}},
				Check: func(ctx context.Context, env *plan.Env) (bool, error) {
					s, err := read(ctx, env, req.Host)
					return err == nil && s.Installed(companion.BackendBaiduPCS), err
				},
				Apply: func(ctx context.Context, env *plan.Env) error {
					_, err := env.Run(ctx, connection.Request{Host: req.Host, Script: companion.BaiduPCSInstallScript, Args: []string{url}, Timeout: 15 * time.Minute, Stall: 5 * time.Minute})
					return err
				},
			})
			p.Warnings = append(p.Warnings, "BaiduPCS-Go publishes no checksums: the binary is fetched over HTTPS from its GitHub releases")
		} else if !current.Installed(b) {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s is not installed on %s: install it with the system's package manager, then rerun this", b, f.Where()))
		}
		if !installable || current.SignedIn(b) {
			continue
		}
		if !req.Login {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s is not signed in on %s: rerun this in a terminal, or sign in there with %s", b, f.Where(), companion.LoginCommand(b)))
			continue
		}
		script, _ := companion.LoginScript(b)
		p.Steps = append(p.Steps, plan.Step{
			ID: "login-" + b, Title: "Sign in to " + b + " on " + f.Where(), Idempotent: true,
			Actions: []plan.Action{{Where: at, Display: companion.LoginCommand(b) + "  (in your terminal; lazylongarm never sees what you type)"}},
			Check: func(ctx context.Context, env *plan.Env) (bool, error) {
				s, err := read(ctx, env, req.Host)
				return err == nil && s.SignedIn(b), err
			},
			Apply: func(ctx context.Context, env *plan.Env) error {
				if env.Interactive == nil {
					return plan.WithFix(fmt.Errorf("signing in to %s needs a terminal", b), OnHost(req.Host, companion.LoginCommand(b)))
				}
				if err := env.Interactive(ctx, connection.Request{Host: req.Host, Script: script, Timeout: time.Hour}); err != nil {
					return err
				}
				s, err := read(ctx, env, req.Host)
				if err != nil {
					return err
				}
				if !s.SignedIn(b) {
					return plan.WithFix(fmt.Errorf("%s is still not signed in on %s", b, f.Where()), OnHost(req.Host, companion.LoginCommand(b)))
				}
				return nil
			},
		})
	}
	if req.Login && len(p.Steps) > 0 {
		p.Warnings = append(p.Warnings, "backend logins run in your terminal (ssh -tt): lazylongarm never reads, stores or copies the credentials")
	}

	p.Steps = append(p.Steps, plan.Step{
		ID: "verify", Title: "Check ferry and the backends on " + f.Where(), Idempotent: true, ReadOnly: true,
		Actions: []plan.Action{{Where: at, Display: "ferry --version; each backend's sign-in state (no account names)"}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			s, err := read(ctx, env, req.Host)
			if err != nil {
				return err
			}
			if s.Ferry == "" {
				return plan.WithFix(fmt.Errorf("ferry is not on PATH on %s after the install", f.Where()), OnHost(req.Host, "~/.local/bin/ferry --version"))
			}
			env.Set("ferry_version", s.Version)
			env.Set("ferry_backends", strings.Join(s.SignedInBackends(), ","))
			env.Logf("  │ ferry %s", dash(s.Version))
			for _, b := range req.Backends {
				env.Logf("  │ %s: %s", b, s.BackendState(b))
			}
			return nil
		},
	})
	p.Steps = append(p.Steps, plan.Step{
		ID: "save-target", Title: "Record the ferry state in lazylongarm's config", Idempotent: true,
		Actions: []plan.Action{{Where: "local", Display: "save ferry version and signed-in backend names on the target (only for machines lazylongarm manages)"}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			if deps.Config == nil {
				return nil
			}
			id := ""
			if err := deps.Config.Update(func(c *config.Config) error {
				t, ok := c.TargetByHost(req.Host)
				if !ok {
					return errNotTarget
				}
				t.FerryVersion = env.Get("ferry_version")
				t.FerryBackends = nil
				if v := env.Get("ferry_backends"); v != "" {
					t.FerryBackends = strings.Split(v, ",")
				}
				t.FerryCheckedAt = time.Now().UTC().Format(time.RFC3339)
				id = t.ID
				return nil
			}); errors.Is(err, errNotTarget) {
				env.Logf("  %s is not a lazylongarm target; nothing recorded", f.Where())
				return nil
			} else if err != nil {
				return err
			}
			env.Logf("  saved ferry state on target %q", id)
			return nil
		},
	})
	return p, nil
}

// ---- companion ferry pair ----

// FerryEnd is one machine of a pair.
type FerryEnd struct {
	Host    string // SSH alias, "" = this machine
	Name    string // display name
	Record  string // id recorded in [[companions.ferry.jobs]]
	OS      string // probed
	JobPath string // absolute path of the job file there
}

// FerryPairRequest asks to write Job on Sender and Receiver.
type FerryPairRequest struct {
	Job      companion.FerryJob // paths already absolute
	Sender   FerryEnd
	Receiver FerryEnd
	// Service installs ferry's systemd user units (receiver, then sender) on
	// Linux machines; the transfer starts with them.
	Service bool
	// RsyncHost is the ssh host the sender pushes to (rsync jobs) and
	// RsyncRoute how ssh on the sender resolves it.
	RsyncHost  string
	RsyncRoute companion.SSHRoute
}

// BuildCompanionFerryPair plans write the job on both machines → ferry
// doctor on each side → ferry plan on the sender → (services) → record.
func BuildCompanionFerryPair(req FerryPairRequest, sender, receiver companion.FerryState, deps FerryDeps) (*plan.Plan, error) {
	backend, err := companion.AdapterBackend(req.Job.Adapter)
	if err != nil {
		return nil, err
	}
	if req.Sender.Host == req.Receiver.Host {
		return nil, errors.New("sender and receiver are the same machine; ferry is for moving data between two machines")
	}
	req.Job.SenderName, req.Job.ReceiverName = req.Sender.Name, req.Receiver.Name
	body, err := req.Job.Render()
	if err != nil {
		return nil, err
	}
	var problems, outdated []string
	for _, side := range []struct {
		end   FerryEnd
		state companion.FerryState
	}{{req.Sender, sender}, {req.Receiver, receiver}} {
		setup := "lazylongarm companion ferry setup " + side.end.Name + " --backend " + backend
		if backend == companion.BackendRsync {
			setup = "lazylongarm companion ferry setup " + side.end.Name
		}
		switch {
		case side.state.Ferry == "":
			problems = append(problems, fmt.Sprintf("ferry is not installed on %s (run: %s)", side.end.Name, setup))
		case !companion.VersionAtLeast(side.state.Version, companion.MinFerryVersion):
			outdated = append(outdated, side.end.Name)
			problems = append(problems, fmt.Sprintf("ferry %s on %s is older than %s", dash(side.state.Version), side.end.Name, companion.MinFerryVersion))
		case backend == companion.BackendRsync && !side.state.Installed(backend):
			problems = append(problems, fmt.Sprintf("rsync is not installed on %s (install it with the system's package manager)", side.end.Name))
		case !side.state.Installed(backend):
			problems = append(problems, fmt.Sprintf("%s is not installed on %s (run: %s)", backend, side.end.Name, setup))
		case !side.state.SignedIn(backend):
			problems = append(problems, fmt.Sprintf("%s is not signed in on %s (run: %s)", backend, side.end.Name, setup))
		case backend == companion.BackendRclone && !side.state.HasRcloneRemote(req.Job.Options["remote"]):
			problems = append(problems, fmt.Sprintf("rclone on %s has no remote %q (add it there with rclone config)", side.end.Name, req.Job.Options["remote"]))
		}
	}
	if len(outdated) > 0 {
		problems = append(problems, "run: lazylongarm companion ferry setup "+strings.Join(outdated, " ")+" --upgrade")
	}
	if len(problems) == 0 && sender.Version != receiver.Version {
		problems = append(problems, fmt.Sprintf("ferry versions differ (%s %s, %s %s): sender and receiver must run the same ferry (run: lazylongarm companion ferry setup %s %s --upgrade)",
			req.Sender.Name, dash(sender.Version), req.Receiver.Name, dash(receiver.Version), req.Sender.Name, req.Receiver.Name))
	}
	if backend == companion.BackendRsync && (req.RsyncHost == "" || req.Job.Options["host"] != req.RsyncHost) {
		problems = append(problems, "an rsync job needs the ssh host the sender pushes to")
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}

	extract := "kept as tar archives under the destination"
	if req.Job.Extract {
		extract = "extracted under the destination (each unit path is replaced whole)"
	}
	relay := req.Job.Adapter + " " + req.Job.Prefix
	for _, k := range slices.Sorted(maps.Keys(req.Job.Options)) {
		relay += fmt.Sprintf(" (%s %s)", k, req.Job.Options[k])
	}
	if backend == companion.BackendRsync {
		relay = fmt.Sprintf("rsync over ssh from %s to %s", req.Sender.Name, req.RsyncHost)
		if h := req.RsyncRoute.HostName; h != "" && h != req.RsyncHost {
			relay += " (" + h + ")"
		}
		relay += ", landing in " + req.Job.Options["root"] + " on " + req.Receiver.Name
	}
	sources := "kept (delete_source = never)"
	if d := req.Job.DeleteSource; d != "" && d != "never" {
		sources = "DELETED on the sender after relaying (delete_source = " + d + ")"
	}
	p := &plan.Plan{
		Kind:  "companion-ferry-pair",
		Key:   req.Job.Name,
		Title: fmt.Sprintf("ferry job %s: %s → %s", req.Job.Name, req.Sender.Name, req.Receiver.Name),
		Summary: [][2]string{
			{"job", req.Job.Name},
			{"from", req.Sender.Name + ":" + req.Job.Root + " (" + req.Job.Glob + ")"},
			{"to", req.Receiver.Name + ":" + req.Job.Dest + ", " + extract},
			{"relay", relay},
			{"sources", sources},
			{"job file", req.Sender.Name + ":" + req.Sender.JobPath + ", " + req.Receiver.Name + ":" + req.Receiver.JobPath},
		},
	}
	if d := req.Job.DeleteSource; d != "" && d != "never" {
		p.Warnings = append(p.Warnings, "delete_source = "+d+": ferry deletes source units on "+req.Sender.Name+" after they are relayed")
	}
	if backend == companion.BackendRsync {
		r := req.RsyncRoute
		if r.ThroughLongarm(req.RsyncHost) {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s reaches %s through Longarm (%s): without a direct path between the two machines the data takes the hub's relay; prefer a direct route (LAN, tailnet) or a cloud relay (--via baidupcs|rclone|ghrelease)", req.Sender.Name, req.RsyncHost, dash(r.ProxyCommand)))
		}
		if r.ProxyJump != "" {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s reaches %s through the jump host %s: the data passes through it", req.Sender.Name, req.RsyncHost, r.ProxyJump))
		}
		p.Warnings = append(p.Warnings, fmt.Sprintf("rsync needs non-interactive ssh (a key) from %s to %s, and %q must name %s there; ferry doctor on the sender checks it", req.Sender.Name, req.RsyncHost, req.RsyncHost, req.Receiver.Name))
	} else {
		p.Warnings = append(p.Warnings, "the data goes through the "+req.Job.Adapter+" relay, not the Longarm hub; backend quotas and the relay's own limits apply")
	}

	for i, end := range []FerryEnd{req.Sender, req.Receiver} {
		display := "write " + end.JobPath + " (refuses to replace a different file)\n" + strings.TrimRight(body, "\n")
		if i > 0 {
			display = "write " + end.JobPath + " (the same content; refuses to replace a different file)"
		}
		p.Steps = append(p.Steps, plan.Step{
			ID: "job-" + end.Name, Title: "Write the job on " + end.Name, Idempotent: true,
			Actions: []plan.Action{{Where: where(end.Host), Display: display}},
			Check: func(ctx context.Context, env *plan.Env) (bool, error) {
				st, err := jobFile(ctx, env, end, body, false)
				if err != nil {
					return false, err
				}
				return st == "same", nil
			},
			Apply: func(ctx context.Context, env *plan.Env) error {
				_, err := jobFile(ctx, env, end, body, true)
				return err
			},
		})
	}
	for _, s := range []struct {
		end  FerryEnd
		side string
	}{{req.Sender, "sender"}, {req.Receiver, "receiver"}} {
		p.Steps = append(p.Steps, plan.Step{
			ID: "doctor-" + s.side, Title: "ferry doctor on " + s.end.Name + " (" + s.side + ")", Idempotent: true, ReadOnly: true,
			Actions: []plan.Action{{Where: where(s.end.Host), Display: "ferry doctor " + s.end.JobPath + " --side " + s.side}},
			Apply: func(ctx context.Context, env *plan.Env) error {
				res, err := env.Run(ctx, connection.Request{Host: s.end.Host, Script: companion.FerryRunScript, Args: []string{"doctor", s.end.JobPath, "--side", s.side}, Timeout: 3 * time.Minute})
				for line := range strings.SplitSeq(strings.TrimSpace(string(res.Stdout)), "\n") {
					if line != "" {
						env.Logf("  │ %s", line)
					}
				}
				if err != nil {
					return plan.WithFix(fmt.Errorf("ferry doctor failed on %s: %w", s.end.Name, err), OnHost(s.end.Host, "ferry doctor "+s.end.JobPath+" --side "+s.side))
				}
				return nil
			},
		})
	}
	p.Steps = append(p.Steps, plan.Step{
		ID: "plan", Title: "List the units on " + req.Sender.Name, Idempotent: true, ReadOnly: true,
		Actions: []plan.Action{{Where: where(req.Sender.Host), Display: "ferry plan " + req.Sender.JobPath + " --json"}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			res, err := env.RunQuiet(ctx, connection.Request{Host: req.Sender.Host, Script: companion.FerryRunScript, Args: []string{"plan", req.Sender.JobPath, "--json"}, Timeout: 5 * time.Minute})
			if err != nil {
				return err
			}
			n, err := companion.FerryPlanUnits(res.Stdout)
			if err != nil {
				return err
			}
			env.Set("units", fmt.Sprint(n))
			env.Logf("  │ %d unit(s) to send", n)
			if n == 0 {
				env.Logf("  ! nothing matches %q under %s", req.Job.Glob, req.Job.Root)
			}
			return nil
		},
	})
	if req.Service {
		for _, s := range []struct {
			end  FerryEnd
			side string
		}{{req.Receiver, "receiver"}, {req.Sender, "sender"}} {
			if s.end.OS != "linux" {
				p.Warnings = append(p.Warnings, fmt.Sprintf("%s is not Linux: ferry has no service installer there; run the %s yourself (see the next steps)", s.end.Name, s.side))
				continue
			}
			p.Steps = append(p.Steps, plan.Step{
				ID: "service-" + s.side, Title: "Install and start the " + s.side + " service on " + s.end.Name, Idempotent: true,
				Actions: []plan.Action{{Where: where(s.end.Host), Display: "ferry service " + s.end.JobPath + " --side " + s.side + " --install  (systemd user unit, starts now)"}},
				Apply: func(ctx context.Context, env *plan.Env) error {
					_, err := env.Run(ctx, connection.Request{Host: s.end.Host, Script: companion.FerryRunScript, Args: []string{"service", s.end.JobPath, "--side", s.side, "--install"}, Timeout: 2 * time.Minute})
					return err
				},
			})
		}
		p.Warnings = append(p.Warnings, "--service starts the transfer as soon as the units are installed; a systemd user unit keeps running after logout only with lingering enabled (loginctl enable-linger)")
	}
	p.Steps = append(p.Steps, plan.Step{
		ID: "save-job", Title: "Record the job in lazylongarm's config", Idempotent: true,
		Actions: []plan.Action{{Where: "local", Display: "[[companions.ferry.jobs]] name, machines, paths, adapter and prefix (no credentials)"}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			if deps.Config == nil {
				return nil
			}
			return deps.Config.Update(func(c *config.Config) error {
				c.Companions.Ferry.UpsertJob(config.FerryJob{
					Name: req.Job.Name, Sender: req.Sender.Record, Receiver: req.Receiver.Record,
					Source: req.Job.Root, Dest: req.Job.Dest, Adapter: req.Job.Adapter, Prefix: req.Job.Prefix,
					CreatedAt: time.Now().UTC().Format(time.RFC3339),
				})
				return nil
			})
		},
	})
	return p, nil
}

// jobFile compares (or writes) the job on end; a different file is an error.
func jobFile(ctx context.Context, env *plan.Env, end FerryEnd, body string, write bool) (string, error) {
	mode := "check"
	if write {
		mode = "write"
	}
	res, err := env.RunQuiet(ctx, connection.Request{Host: end.Host, Script: companion.JobFileScript, Args: []string{end.JobPath, mode}, Stdin: []byte(body), Timeout: 30 * time.Second})
	if err != nil {
		return "", err
	}
	st, err := companion.ParseJobFileState(string(res.Stdout))
	if err != nil {
		return "", err
	}
	if st == "differs" {
		return st, plan.WithFix(fmt.Errorf("%s already has a different %s; lazylongarm will not replace it", end.Name, end.JobPath), "pick another --name, or remove that file after checking it is not in use")
	}
	return st, nil
}

// ReadPathKind reports whether p on host is a dir, file, other or missing.
func ReadPathKind(ctx context.Context, env *plan.Env, host, p string) (string, error) {
	res, err := env.RunQuiet(ctx, connection.Request{Host: host, Script: companion.PathKindScript, Args: []string{p}, Timeout: 30 * time.Second})
	if err != nil {
		return "", err
	}
	if k := companion.ParsePathKind(string(res.Stdout)); k != "" {
		return k, nil
	}
	return "", errors.New("no path kind reported")
}

// CompanionState is the dotfiles and ferry state of one machine, read in a
// single SSH round trip (`companion status`).
type CompanionState struct {
	Dotfiles    companion.DotfilesState
	DotfilesErr error
	Ferry       companion.FerryState
	FerryErr    error
}

const companionStateSeparator = "__lazylongarm_ferry__"

// ReadCompanionState runs companion.StateScript and companion.FerryStateScript
// on host over one connection. A transport error is returned as err; a report
// that does not parse is that part's error.
func ReadCompanionState(ctx context.Context, env *plan.Env, host string) (CompanionState, error) {
	script := "(\n" + companion.StateScript + "\n)\necho " + companionStateSeparator + "\n" + companion.FerryStateScript
	res, err := env.RunQuiet(ctx, connection.Request{Host: host, Script: script, Timeout: 30 * time.Second})
	if err != nil {
		return CompanionState{}, err
	}
	dot, fer, _ := strings.Cut(string(res.Stdout), companionStateSeparator+"\n")
	var s CompanionState
	s.Dotfiles, s.DotfilesErr = companion.ParseState(dot)
	s.Ferry, s.FerryErr = companion.ParseFerryState(fer)
	return s, nil
}

// ReadSSHRoute reports how ssh on host resolves target (ssh -G).
func ReadSSHRoute(ctx context.Context, env *plan.Env, host, target string) (companion.SSHRoute, error) {
	res, err := env.RunQuiet(ctx, connection.Request{Host: host, Script: companion.SSHRouteScript, Args: []string{target}, Timeout: 30 * time.Second})
	if err != nil {
		return companion.SSHRoute{}, err
	}
	return companion.ParseSSHRoute(string(res.Stdout)), nil
}
