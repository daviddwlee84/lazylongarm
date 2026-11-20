package flows

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/companion"
	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/hostprobe"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
)

// DotfilesRequest asks to bootstrap (or update) chezmoi dotfiles on Host.
// The CLI resolves Repo, Bootstrap and Bundle from flags and the config.
type DotfilesRequest struct {
	Host      string // SSH alias, "" = this machine
	Name      string // display name (the target's name)
	Repo      string
	Bootstrap string // template; "" = the built-in chezmoi install + init
	Bundle    string
	Update    bool // run `chezmoi update` when already initialized
	// Answers says how the built-in bootstrap answers chezmoi's prompts
	// (zero value = ask). Templates answer prompts themselves.
	Answers companion.Answers
	// Interactive: the apply will run attached to a terminal.
	Interactive bool
}

// DotfilesDeps: the config to record the result in, and the state reader
// (a test seam; nil runs companion.StateScript on the host).
type DotfilesDeps struct {
	Config *config.Config
	State  func(ctx context.Context, env *plan.Env, host string) (companion.DotfilesState, error)
}

// ReadDotfilesState runs companion.StateScript on host.
func ReadDotfilesState(ctx context.Context, env *plan.Env, host string) (companion.DotfilesState, error) {
	res, err := env.RunQuiet(ctx, connection.Request{Host: host, Script: companion.StateScript, Timeout: 30 * time.Second})
	if err != nil {
		return companion.DotfilesState{}, err
	}
	return companion.ParseState(string(res.Stdout))
}

// DotfilesValues are the template placeholder values for req on f.
func DotfilesValues(req DotfilesRequest, f *hostprobe.Facts) map[string]string {
	return map[string]string{"repo": req.Repo, "bundle": req.Bundle, "name": req.Name, "os": f.OS, "arch": f.Arch, "distro": f.DistroID}
}

// BuildCompanionDotfiles plans bootstrap (or update) → verify → record.
// current is the state read while planning; steps re-read it when applying.
func BuildCompanionDotfiles(req DotfilesRequest, f *hostprobe.Facts, current companion.DotfilesState, deps DotfilesDeps) (*plan.Plan, error) {
	if deps.State == nil {
		deps.State = ReadDotfilesState
	}
	if req.Name == "" {
		req.Name = req.Host
		if req.Name == "" {
			req.Name = f.Hostname
		}
	}
	where := req.Host
	if where == "" {
		where = "local"
	}
	custom := strings.TrimSpace(req.Bootstrap) != ""
	values := DotfilesValues(req, f)
	// An initialized machine needs no repository or template: it is skipped
	// or updated from its own source.
	fresh := !current.Initialized()
	var problems []string
	if fresh && !custom && req.Repo == "" {
		problems = append(problems, "no dotfiles repository: pass --repo or set [companions.dotfiles] repo in the config")
	}
	if fresh && !custom && !f.Has["curl"] && !f.Has["wget"] && !f.Has["chezmoi"] {
		problems = append(problems, f.Where()+" has neither curl nor wget to install chezmoi")
	}
	if fresh && !f.Has["git"] {
		problems = append(problems, f.Where()+" has no git (chezmoi needs it to clone the repository)")
	}
	if req.Answers.Mode == "" {
		req.Answers.Mode = companion.AnswersAsk
	}
	initFlags := req.Answers.InitFlags(req.Interactive)
	if fresh && !custom && req.Answers.Mode == companion.AnswersAsk && !req.Interactive {
		problems = append(problems, "answers=ask needs a terminal for chezmoi's prompts: run it in a terminal, or pass --answers defaults or a preset")
	}
	body, err := companion.BootstrapScript(req.Bootstrap, values, initFlags)
	if err != nil && (custom || (fresh && req.Repo != "")) {
		problems = append(problems, "bootstrap template: "+err.Error())
	}
	if err != nil {
		body = ""
	}
	if current.Initialized() && req.Repo != "" && current.Origin != "" && !companion.SameRepo(current.Origin, req.Repo) {
		problems = append(problems, fmt.Sprintf("%s already uses dotfiles from %s, not %s; lazylongarm will not replace them", f.Where(), current.Origin, req.Repo))
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}

	method := "built-in: install chezmoi if missing, then chezmoi init --apply"
	switch {
	case !fresh && req.Update:
		method = "chezmoi update of the existing source"
	case !fresh:
		method = "none needed"
	case custom:
		method = "bootstrap template from the config"
	}
	state := "not set up"
	switch {
	case current.Initialized():
		state = fmt.Sprintf("set up (%s @ %s)", dash(current.Origin), dash(current.Rev))
	case current.Chezmoi != "":
		state = "chezmoi installed, no source yet"
	}
	p := &plan.Plan{
		Kind:  "companion-dotfiles",
		Key:   where,
		Title: fmt.Sprintf("Dotfiles on %s", f.Where()),
		Summary: [][2]string{
			{"machine", fmt.Sprintf("%s · %s · %s", f.Where(), f.Platform(), f.Distro)},
			{"repository", dash(req.Repo)},
			{"bundle", dash(req.Bundle)},
			{"method", method},
			{"answers", answersLabel(req, custom, fresh)},
			{"current", state},
		},
	}
	update := current.Initialized() && req.Update
	updateDefaults := req.Answers.Mode == companion.AnswersDefaults || !req.Interactive
	title, display := "Bootstrap dotfiles", ""
	switch {
	case update:
		title, display = "Update dotfiles (chezmoi update)", companion.UpdateCommand(updateDefaults)
	case !fresh:
		display = "nothing (already set up; --update pulls and applies)"
	case custom:
		display = strings.TrimSpace(strings.TrimPrefix(body, companion.RemotePath+"\n"))
	default:
		display = "install chezmoi into ~/.local/bin when missing (get.chezmoi.io)\n" + companion.InitCommand(req.Repo, initFlags)
	}
	if fresh && !custom && req.Answers.Mode == "preset" && !req.Interactive {
		p.Warnings = append(p.Warnings, "no terminal: prompts the preset does not answer take their defaults")
	}
	if current.Initialized() && !update {
		p.Warnings = append(p.Warnings, "dotfiles are already set up on "+f.Where()+"; pass --update to pull and apply the latest")
	}
	p.Warnings = append(p.Warnings, "the bootstrap runs in your terminal (ssh -tt): answer sudo or other prompts there; it may take several minutes")

	p.Steps = append(p.Steps, plan.Step{
		ID: "bootstrap", Title: title, Idempotent: true,
		Actions: []plan.Action{{Where: where, Display: display}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			s, err := deps.State(ctx, env, req.Host)
			if err != nil {
				return false, err
			}
			if !s.Initialized() {
				return false, nil
			}
			if req.Repo != "" && s.Origin != "" && !companion.SameRepo(s.Origin, req.Repo) {
				return false, fmt.Errorf("%s now uses dotfiles from %s; refusing to replace them", f.Where(), s.Origin)
			}
			return !req.Update, nil
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			s, err := deps.State(ctx, env, req.Host)
			if err != nil {
				return err
			}
			script := body
			if s.Initialized() {
				script = companion.UpdateScript(updateDefaults)
			} else if script == "" {
				return fmt.Errorf("dotfiles are no longer set up on %s and no repository or bootstrap template is configured", f.Where())
			}
			r := connection.Request{Host: req.Host, Script: script, Timeout: 2 * time.Hour}
			if env.Interactive != nil {
				return env.Interactive(ctx, r)
			}
			_, err = env.Run(ctx, r)
			return err
		},
	})
	p.Steps = append(p.Steps, plan.Step{
		ID: "verify", Title: "Check the chezmoi source on " + f.Where(), Idempotent: true, ReadOnly: true,
		Actions: []plan.Action{{Where: where, Display: "chezmoi source-path; git rev-parse --short HEAD"}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			s, err := deps.State(ctx, env, req.Host)
			if err != nil {
				return err
			}
			if !s.Initialized() {
				return plan.WithFix(fmt.Errorf("chezmoi has no source checkout on %s after the bootstrap", f.Where()), "ssh "+dash(req.Host)+" chezmoi doctor")
			}
			env.Set("dotfiles_rev", s.Rev)
			env.Set("dotfiles_origin", s.Origin)
			env.Logf("  │ %s @ %s", dash(s.Origin), dash(s.Rev))
			if req.Repo != "" && s.Origin != "" && !companion.SameRepo(s.Origin, req.Repo) {
				env.Logf("  ! the source comes from %s, not %s", s.Origin, req.Repo)
			}
			return nil
		},
	})
	p.Steps = append(p.Steps, plan.Step{
		ID: "save-target", Title: "Record the dotfiles state in lazylongarm's config", Idempotent: true,
		Actions: []plan.Action{{Where: "local", Display: "save bundle and revision on the target (only for machines lazylongarm manages)"}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			if deps.Config == nil {
				return nil
			}
			t, ok := deps.Config.TargetByHost(req.Host)
			if !ok {
				env.Logf("  %s is not a lazylongarm target; nothing recorded", f.Where())
				return nil
			}
			if req.Bundle != "" {
				t.DotfilesBundle = req.Bundle
			}
			t.DotfilesRev = env.Get("dotfiles_rev")
			t.DotfilesAppliedAt = time.Now().UTC().Format(time.RFC3339)
			if err := deps.Config.Save(); err != nil {
				return err
			}
			env.Logf("  saved dotfiles state on target %q", t.ID)
			return nil
		},
	})
	return p, nil
}

func answersLabel(req DotfilesRequest, custom, fresh bool) string {
	switch {
	case custom:
		return "the bootstrap template answers them"
	case !fresh && req.Update && (req.Answers.Mode == companion.AnswersDefaults || !req.Interactive):
		return "new prompts take their defaults"
	case !fresh && req.Update:
		return "new prompts are asked in your terminal"
	case !fresh:
		return "-"
	}
	return req.Answers.Label()
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
