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

// RemoteTarget is a fleet machine to register in hc/herdr.
type RemoteTarget struct {
	Name    string
	SSHHost string
}

// RemotesRequest asks to register Targets in Tools (hc, herdr).
type RemotesRequest struct {
	Targets []RemoteTarget
	Tools   []string
}

// RemotesDeps carries the local tool adapter and the lists read before
// planning (tool → machines; a tool absent from Lists is not installed).
type RemotesDeps struct {
	Tools companion.Tools
	Lists map[string][]companion.Remote
}

// BuildCompanionRemotes plans one local step per (tool, machine) that is not
// registered yet. Machines already registered are listed in the summary.
func BuildCompanionRemotes(req RemotesRequest, deps RemotesDeps) (*plan.Plan, error) {
	if len(req.Targets) == 0 {
		return nil, fmt.Errorf("no machines selected")
	}
	if len(req.Tools) == 0 {
		req.Tools = companion.RemoteTools
	}
	p := &plan.Plan{
		Kind:  "companion-remotes",
		Key:   "local",
		Title: "Register fleet machines in " + strings.Join(req.Tools, " and "),
	}
	var names []string
	for _, t := range req.Targets {
		if t.SSHHost == "" {
			return nil, fmt.Errorf("%s has no SSH host (this machine needs no remote entry)", t.Name)
		}
		names = append(names, t.Name)
	}
	p.Summary = append(p.Summary, [2]string{"machines", strings.Join(names, ", ")})
	for _, tool := range req.Tools {
		list, installed := deps.Lists[tool]
		if !installed {
			p.Summary = append(p.Summary, [2]string{tool, "not installed here: skipped"})
			p.Warnings = append(p.Warnings, tool+" is not installed on this machine; install it, then run this again to register the machines")
			continue
		}
		var present []string
		for _, t := range req.Targets {
			if r, ok := companion.Find(list, t.SSHHost, t.Name); ok {
				present = append(present, t.Name)
				if !r.Enabled {
					p.Warnings = append(p.Warnings, fmt.Sprintf("%s: %s is registered but disabled (`herdr machine enable %s`)", tool, t.Name, r.Name))
				}
				continue
			}
			p.Steps = append(p.Steps, remoteStep(tool, t, deps.Tools))
		}
		state := fmt.Sprintf("%d machines known", len(list))
		if len(present) > 0 {
			state += "; already registered: " + strings.Join(present, ", ")
		}
		p.Summary = append(p.Summary, [2]string{tool, state})
	}
	return p, nil
}

func remoteStep(tool string, t RemoteTarget, tools companion.Tools) plan.Step {
	argv := companion.AddArgv(tool, t.SSHHost, t.Name)
	display := make([]string, len(argv))
	for i, a := range argv {
		display[i] = displayArg(a)
	}
	note := map[string]string{
		companion.HC:    "copies hc to the machine when it has none or an old one",
		companion.Herdr: "installs and starts the herdr server there; approve its prompts in the terminal",
	}[tool]
	listed := func(ctx context.Context) (bool, error) {
		list, err := tools.List(ctx, tool)
		if err != nil {
			return false, err
		}
		_, ok := companion.Find(list, t.SSHHost, t.Name)
		return ok, nil
	}
	return plan.Step{
		ID:         tool + "-" + config.Slug(t.Name),
		Title:      fmt.Sprintf("Register %s in %s", t.Name, tool),
		Idempotent: true,
		Actions:    []plan.Action{{Where: "local", Display: strings.Join(display, " ") + "\n(" + note + ")"}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			return listed(ctx)
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			req := connection.Request{Script: `exec "$@"`, Args: argv, Timeout: 15 * time.Minute}
			if env.Interactive != nil {
				return env.Interactive(ctx, req)
			}
			_, err := env.Run(ctx, req)
			return err
		},
		Verify: func(ctx context.Context, env *plan.Env) error {
			ok, err := listed(ctx)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%s does not list %s after adding it", tool, t.Name)
			}
			return nil
		},
	}
}

func displayArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"$`\\;&|<>*?()[]{}") {
		return s
	}
	return connection.ShellQuote(s)
}
