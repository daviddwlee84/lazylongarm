package flows

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daviddwlee84/lazylongarm/internal/companion"
	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
)

func TestCompanionRemotesPlansOnlyMissing(t *testing.T) {
	lists := map[string][]companion.Remote{
		companion.HC: {{Name: "worker-1", Target: "worker-1", Enabled: true}},
		// herdr not installed
	}
	p, err := BuildCompanionRemotes(RemotesRequest{Targets: []RemoteTarget{{"worker-1", "worker-1"}, {"worker-2", "me@worker-2.example.com"}}}, RemotesDeps{Lists: lists})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(p); got != "hc-worker-2" {
		t.Fatalf("steps %s", got)
	}
	var b strings.Builder
	p.Render(&b)
	for _, s := range []string{"hc machine add me@worker-2.example.com --install --name worker-2", "already registered: worker-1", "herdr is not installed"} {
		if !strings.Contains(b.String(), s) {
			t.Errorf("review lacks %q:\n%s", s, b.String())
		}
	}
	if _, err := BuildCompanionRemotes(RemotesRequest{Targets: []RemoteTarget{{"me", ""}}}, RemotesDeps{}); err == nil {
		t.Fatal("local target accepted")
	}
}

func TestCompanionRemotesApplyRegistersAndVerifies(t *testing.T) {
	registered := false
	tools := companion.Tools{
		LookPath: func(string) (string, error) { return "/bin/x", nil },
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if registered {
				return []byte(`[{"label":"w","target":"w","enabled":true}]`), nil
			}
			return []byte(`[]`), nil
		},
	}
	p, err := BuildCompanionRemotes(RemotesRequest{Targets: []RemoteTarget{{"w", "w"}}, Tools: []string{companion.Herdr}}, RemotesDeps{Tools: tools, Lists: map[string][]companion.Remote{companion.Herdr: nil}})
	if err != nil {
		t.Fatal(err)
	}
	env := plan.NewEnv()
	var ran []string
	env.Interactive = func(_ context.Context, req connection.Request) error {
		ran = req.Args
		registered = true
		return nil
	}
	results, err := (&plan.Runner{}).Run(context.Background(), p, env, p.Digest())
	if err != nil || results[0].Status != plan.Done {
		t.Fatalf("%v %+v", err, results)
	}
	if strings.Join(ran, " ") != "herdr machine add w --label w" {
		t.Fatalf("ran %v", ran)
	}
	results, err = (&plan.Runner{}).Run(context.Background(), p, env, p.Digest())
	if err != nil || results[0].Status != plan.Skipped {
		t.Fatalf("rerun should skip: %v %+v", err, results)
	}
}

func dotfilesCfg(t *testing.T) *config.Config {
	t.Helper()
	c, _ := config.Load(filepath.Join(t.TempDir(), "c.toml"), false)
	if err := c.Update(func(c *config.Config) error {
		c.UpsertTarget(config.Target{ID: "worker-1", Name: "worker-1", SSHHost: "worker-1"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCompanionDotfilesBootstrapRecordsState(t *testing.T) {
	f := ubuntu()
	f.Has["git"] = true
	f.Host = "worker-1"
	cfg := dotfilesCfg(t)
	state := companion.DotfilesState{}
	deps := DotfilesDeps{Config: cfg, State: func(context.Context, *plan.Env, string) (companion.DotfilesState, error) { return state, nil }}
	req := DotfilesRequest{Host: "worker-1", Name: "worker-1", Repo: "https://github.com/you/dotfiles.git", Bundle: "server", Interactive: true}
	p, err := BuildCompanionDotfiles(req, f, state, deps)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(p); got != "bootstrap,verify,save-target" {
		t.Fatalf("steps %s", got)
	}
	js, _ := json.Marshal(p.JSON())
	if !strings.Contains(string(js), "chezmoi init --apply 'https://github.com/you/dotfiles.git'") || !strings.Contains(string(js), "ask: chezmoi prompts in your terminal") {
		t.Fatalf("dry-run json: %s", js)
	}
	env := plan.NewEnv()
	var script string
	env.Interactive = func(_ context.Context, r connection.Request) error {
		script = r.Script
		state = companion.DotfilesState{Chezmoi: "/x/chezmoi", Source: "/s", Origin: "git@github.com:you/dotfiles.git", Rev: "abc1234"}
		return nil
	}
	if _, err := (&plan.Runner{}).Run(context.Background(), p, env, p.Digest()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "get.chezmoi.io") || !strings.HasPrefix(script, companion.RemotePath) || strings.Contains(script, "promptDefaults") {
		t.Fatalf("script %s", script)
	}
	tg, _ := cfg.TargetByHost("worker-1")
	if tg.DotfilesRev != "abc1234" || tg.DotfilesBundle != "server" || tg.DotfilesAppliedAt == "" {
		t.Fatalf("target %+v", tg)
	}
	// Rerun: already set up → skipped without running anything.
	script = ""
	results, err := (&plan.Runner{}).Run(context.Background(), p, env, p.Digest())
	if err != nil || results[0].Status != plan.Skipped || script != "" {
		t.Fatalf("rerun: %v %+v %q", err, results, script)
	}
}

func TestCompanionDotfilesTemplateUpdateAndRefusals(t *testing.T) {
	f := ubuntu()
	f.Has["git"] = true
	tmpl := "curl -fsSL https://example.com/bootstrap.sh | bash -s -- --yes --bundle {bundle}"
	p, err := BuildCompanionDotfiles(DotfilesRequest{Host: "h", Bootstrap: tmpl, Bundle: "cloud vm"}, f, companion.DotfilesState{}, DotfilesDeps{})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	p.Render(&b)
	if !strings.Contains(b.String(), "--bundle 'cloud vm'") || strings.Contains(b.String(), "PATH=") {
		t.Fatalf("review:\n%s", b.String())
	}
	if _, err := BuildCompanionDotfiles(DotfilesRequest{Host: "h", Bootstrap: tmpl}, f, companion.DotfilesState{}, DotfilesDeps{}); err == nil || !strings.Contains(err.Error(), "{bundle} is used but has no value") {
		t.Fatalf("missing bundle: %v", err)
	}
	if _, err := BuildCompanionDotfiles(DotfilesRequest{Host: "h"}, f, companion.DotfilesState{}, DotfilesDeps{}); err == nil || !strings.Contains(err.Error(), "no dotfiles repository") {
		t.Fatalf("missing repo: %v", err)
	}
	other := companion.DotfilesState{Source: "/s", Origin: "https://github.com/someone/dotfiles"}
	if _, err := BuildCompanionDotfiles(DotfilesRequest{Host: "h", Repo: "you"}, f, other, DotfilesDeps{}); err == nil || !strings.Contains(err.Error(), "will not replace") {
		t.Fatalf("other repo: %v", err)
	}
	mine := companion.DotfilesState{Source: "/s", Origin: "https://github.com/you/dotfiles.git", Rev: "1"}
	p, err = BuildCompanionDotfiles(DotfilesRequest{Host: "h", Repo: "you", Update: true}, f, mine, DotfilesDeps{State: func(context.Context, *plan.Env, string) (companion.DotfilesState, error) { return mine, nil }})
	if err != nil || p.Steps[0].Title != "Update dotfiles (chezmoi update)" {
		t.Fatalf("update: %v %+v", err, p)
	}
	env := plan.NewEnv()
	var script string
	env.Interactive = func(_ context.Context, r connection.Request) error { script = r.Script; return nil }
	if _, err := (&plan.Runner{}).Run(context.Background(), p, env, p.Digest()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "chezmoi update --init") {
		t.Fatalf("update script %q", script)
	}
	noGit := ubuntu()
	if _, err := BuildCompanionDotfiles(DotfilesRequest{Host: "h", Repo: "you"}, noGit, companion.DotfilesState{}, DotfilesDeps{}); err == nil || !strings.Contains(err.Error(), "no git") {
		t.Fatalf("no git: %v", err)
	}
}

func TestCompanionDotfilesUpdateNeedsNoRepo(t *testing.T) {
	mine := companion.DotfilesState{Source: "/s", Origin: "git@github.com:you/dotfiles.git", Rev: "1"}
	p, err := BuildCompanionDotfiles(DotfilesRequest{Host: "h", Update: true}, ubuntu(), mine, DotfilesDeps{})
	if err != nil || p.Steps[0].Title != "Update dotfiles (chezmoi update)" {
		t.Fatalf("%v %+v", err, p)
	}
	p, err = BuildCompanionDotfiles(DotfilesRequest{Host: "h"}, ubuntu(), mine, DotfilesDeps{})
	if err != nil || !strings.Contains(p.Steps[0].Actions[0].Display, "already set up") {
		t.Fatalf("skip plan: %v %+v", err, p)
	}
}

func TestCompanionDotfilesAnswerModes(t *testing.T) {
	f := ubuntu()
	f.Has["git"] = true
	none := companion.DotfilesState{}
	if _, err := BuildCompanionDotfiles(DotfilesRequest{Host: "h", Repo: "you"}, f, none, DotfilesDeps{}); err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Fatalf("ask without terminal: %v", err)
	}
	preset, err := companion.ResolveAnswers("server", "", "", map[string]map[string]any{"server": {"profile": "ubuntu_server"}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := BuildCompanionDotfiles(DotfilesRequest{Host: "h", Repo: "you", Answers: preset}, f, none, DotfilesDeps{})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	p.Render(&b)
	for _, want := range []string{"--promptString 'profile=ubuntu_server' --promptChoice 'profile=ubuntu_server' --promptDefaults 'you'", "prompts the preset does not answer take their defaults", `preset "server"`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("review lacks %q:\n%s", want, b.String())
		}
	}
	p, err = BuildCompanionDotfiles(DotfilesRequest{Host: "h", Repo: "you", Answers: companion.Answers{Mode: companion.AnswersDefaults}, Interactive: true}, f, none, DotfilesDeps{})
	if err != nil || !strings.Contains(p.Steps[0].Actions[0].Display, "init --apply --promptDefaults 'you'") {
		t.Fatalf("defaults: %v %+v", err, p.Steps[0].Actions)
	}
	mine := companion.DotfilesState{Source: "/s", Origin: "https://github.com/you/dotfiles"}
	p, err = BuildCompanionDotfiles(DotfilesRequest{Host: "h", Update: true, Interactive: true}, f, mine, DotfilesDeps{})
	if err != nil || p.Steps[0].Actions[0].Display != "chezmoi update --init" {
		t.Fatalf("interactive update asks new prompts: %v %+v", err, p.Steps[0].Actions)
	}
}
