package flows

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/companion"
	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
)

// Transports for `companion remotes`.
const (
	TransportSSH     = "ssh"     // the machine's own SSH host
	TransportLongarm = "longarm" // Host longarm-<worker> through `longarm proxy`
)

// RemoteTarget is a fleet machine to register in hc/herdr.
type RemoteTarget struct {
	Name    string
	SSHHost string // TransportSSH: how ssh reaches it
	Worker  string // TransportLongarm: its Longarm worker name
}

// RemotesRequest asks to register Targets in Tools (hc, herdr).
type RemotesRequest struct {
	Targets   []RemoteTarget
	Tools     []string
	Transport string // TransportSSH (default) | TransportLongarm
	// Switch replaces an entry that reaches the machine another way (same
	// name or SSH host) instead of leaving the machine as it is.
	Switch bool
	// SSH is the alias request for TransportLongarm (its Workers are filled
	// from Targets).
	SSH SSHConfigRequest
}

// RemotesDeps carries the local tool adapter and the lists read before
// planning (tool → machines; a tool absent from Lists is not installed).
type RemotesDeps struct {
	Tools    companion.Tools
	Lists    map[string][]companion.Remote
	SSHState SSHConfigState // TransportLongarm
	SSH      SSHConfigDeps  // TransportLongarm
}

// BuildCompanionRemotes plans one local step per (tool, machine) that is not
// registered yet. Machines already registered are listed in the summary.
// With TransportLongarm the machines are registered as longarm-<worker>,
// after the steps that make ssh resolve those aliases.
func BuildCompanionRemotes(req RemotesRequest, deps RemotesDeps) (*plan.Plan, error) {
	if len(req.Targets) == 0 {
		return nil, fmt.Errorf("no machines selected")
	}
	if len(req.Tools) == 0 {
		req.Tools = companion.RemoteTools
	}
	if req.Transport == "" {
		req.Transport = TransportSSH
	}
	longarm := req.Transport == TransportLongarm
	if !longarm && req.Transport != TransportSSH {
		return nil, fmt.Errorf("unknown transport %q (use ssh, longarm)", req.Transport)
	}
	p := &plan.Plan{
		Kind:  "companion-remotes",
		Key:   "local",
		Title: "Register fleet machines in " + strings.Join(req.Tools, " and "),
	}
	if longarm {
		p.Title += " through Longarm"
	}
	var names []string
	req.SSH.Workers = nil
	for _, t := range req.Targets {
		switch {
		case longarm && t.Worker == "":
			return nil, fmt.Errorf("%s has no Longarm worker name", t.Name)
		case longarm:
			if err := companion.ValidWorkerName(t.Worker); err != nil {
				return nil, err
			}
			req.SSH.Workers = append(req.SSH.Workers, t.Worker)
		case t.SSHHost == "":
			return nil, fmt.Errorf("%s has no SSH host (this machine needs no remote entry)", t.Name)
		}
		names = append(names, t.Name)
	}
	p.Summary = append(p.Summary, [2]string{"machines", strings.Join(names, ", ")})
	if longarm {
		p.Summary = append(p.Summary, [2]string{"transport", "longarm (ssh to longarm-<worker>, keyless, through the hub's mTLS channel)"})
		if err := addSSHConfigSteps(p, req.SSH, deps.SSHState, deps.SSH); err != nil {
			return nil, err
		}
	}
	for _, tool := range req.Tools {
		list, installed := deps.Lists[tool]
		if !installed {
			p.Summary = append(p.Summary, [2]string{tool, "not installed here: skipped"})
			p.Warnings = append(p.Warnings, tool+" is not installed on this machine; install it, then run this again to register the machines")
			continue
		}
		var present, other []string
		for _, t := range req.Targets {
			host, find := t.SSHHost, func(l []companion.Remote) (companion.Remote, bool) { return companion.Find(l, t.SSHHost, t.Name) }
			if longarm {
				host = companion.LongarmAlias(t.Worker)
				find = func(l []companion.Remote) (companion.Remote, bool) { return companion.FindLongarm(l, t.Worker) }
			}
			match := func(l []companion.Remote) bool { _, ok := find(l); return ok }
			if r, ok := find(list); ok {
				present = append(present, t.Name)
				if !r.Enabled {
					p.Warnings = append(p.Warnings, fmt.Sprintf("%s: %s is registered but disabled (`herdr machine enable %s`)", tool, t.Name, r.ID))
				}
				continue
			}
			if longarm {
				if r, ok := companion.Find(list, t.SSHHost, t.Name); ok {
					if !req.Switch {
						other = append(other, fmt.Sprintf("%s (as %s)", t.Name, r.Target))
						continue
					}
					p.Steps = append(p.Steps, forgetStep(tool, t.Name, r))
				}
			}
			p.Steps = append(p.Steps, remoteStep(tool, t.Name, host, match, deps.Tools))
		}
		state := fmt.Sprintf("%d machines known", len(list))
		if len(present) > 0 {
			state += "; already registered: " + strings.Join(present, ", ")
		}
		p.Summary = append(p.Summary, [2]string{tool, state})
		if len(other) > 0 {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s already reaches %s another way; left as is (--switch replaces those entries with longarm ones)", tool, strings.Join(other, ", ")))
		}
	}
	return p, nil
}

// forgetStep drops the entry r that reaches the machine another way, so the
// longarm entry can take its place (hc refuses a second entry for the same
// machine). Its terminals keep running on the machine.
func forgetStep(tool, name string, r companion.Remote) plan.Step {
	argv := companion.RemoveArgv(tool, r)
	return plan.Step{
		ID:         tool + "-forget-" + config.Slug(name),
		Title:      fmt.Sprintf("Forget %s's %s entry %s (%s)", name, tool, r.Name, r.Target),
		Idempotent: true,
		Actions:    []plan.Action{{Where: "local", Display: displayArgv(argv) + "\n(its terminals keep running on the machine)"}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			if tool == companion.Herdr && r.ID == "" {
				return fmt.Errorf("herdr did not list a profile id for %s", r.Name)
			}
			_, err := env.Run(ctx, connection.Request{Script: `exec "$@"`, Args: argv, Timeout: 2 * time.Minute})
			return err
		},
	}
}

func remoteStep(tool, name, host string, listedIn func([]companion.Remote) bool, tools companion.Tools) plan.Step {
	argv := companion.AddArgv(tool, host, name)
	note := map[string]string{
		companion.HC:    "copies hc to the machine when it has none or an old one",
		companion.Herdr: "installs and starts the herdr server there; approve its prompts in the terminal",
	}[tool]
	listed := func(ctx context.Context) (bool, error) {
		list, err := tools.List(ctx, tool)
		if err != nil {
			return false, err
		}
		return listedIn(list), nil
	}
	return plan.Step{
		ID:         tool + "-" + config.Slug(name),
		Title:      fmt.Sprintf("Register %s in %s", name, tool),
		Idempotent: true,
		Actions:    []plan.Action{{Where: "local", Display: displayArgv(argv) + "\n(" + note + ")"}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			return listed(ctx)
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			req := connection.Request{Script: `exec "$@"`, Args: argv, Timeout: 15 * time.Minute}
			if env.Interactive != nil {
				return env.Interactive(ctx, req)
			}
			_, err := env.Run(ctx, req)
			if err != nil && tool == companion.Herdr {
				// herdr asks before installing itself on the machine and
				// refuses without a terminal; never answer for the user.
				return plan.WithFix(err, "run `lazylongarm companion remotes "+name+" --tool herdr` in a terminal to approve herdr's install")
			}
			if err != nil && tool == companion.HC && strings.Contains(err.Error(), "already_added") {
				return plan.WithFix(err, "hc already knows this machine under another entry; `hc machine rm <name>` it, or rerun with --switch")
			}
			return err
		},
		Verify: func(ctx context.Context, env *plan.Env) error {
			ok, err := listed(ctx)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%s does not list %s after adding it", tool, name)
			}
			return nil
		},
	}
}

func displayArgv(argv []string) string {
	display := make([]string, len(argv))
	for i, a := range argv {
		display[i] = displayArg(a)
	}
	return strings.Join(display, " ")
}

func displayArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"$`\\;&|<>*?()[]{}") {
		return s
	}
	return connection.ShellQuote(s)
}
