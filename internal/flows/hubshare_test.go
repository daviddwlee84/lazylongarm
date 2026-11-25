package flows

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/hubshare"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
)

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl me@laptop"

func shareFixture() (ShareRequest, *ShareFacts) {
	hub := &config.Hub{ID: "h", Mode: "public", Address: "hub.example.com:7443", CAFingerprint: "sha256:aa", SSHHost: "hub-box"}
	k, _ := hubshare.ParsePublicKey(testKey)
	tgt := connection.SSHTarget{Hostname: "hub.example.com", User: "admin", Port: "22"}
	return ShareRequest{Host: "laptop-2", Hub: hub}, &ShareFacts{
		Bin: "/opt/homebrew/bin/lazylongarm", BinVersion: "lazylongarm v0.5.0", HasImport: true,
		Membership: hubshare.Membership{Address: "hub.example.com:7443", CAFingerprint: "aa"},
		Local:      tgt, Remote: connection.SSHTarget{Hostname: "hub-box", Port: "22"}, SSHHost: "admin@hub.example.com",
		KnownHosts: []string{"hub.example.com " + strings.SplitN(testKey, " ", 3)[0] + " " + strings.SplitN(testKey, " ", 3)[1]},
		PubKey:     &k, PubKeyFile: "~/.ssh/id_ed25519.pub",
		TargetHostnames:  map[string]string{"w1": "10.0.0.1", "w2": "w2"},
		LocalTargetHosts: map[string]string{"w1": "10.0.0.1", "w2": "10.0.0.2"},
	}
}

func TestHubSharePlan(t *testing.T) {
	req, f := shareFixture()
	req.Targets = true
	targets := []config.Target{
		{ID: "local", Hub: "h"},                         // this machine: skipped
		{ID: "laptop-2", SSHHost: "laptop-2", Hub: "h"}, // the machine itself: becomes local there
		{ID: "w1", SSHHost: "w1", Hub: "h"},
		{ID: "w2", SSHHost: "w2", Hub: "h"},   // resolves differently there: skipped
		{ID: "x", SSHHost: "x", Hub: "other"}, // another hub
	}
	p, err := BuildHubShare(req, f, targets)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(p); got != "hub-host-key,hub-authorize,import,verify" {
		t.Fatalf("steps %s", got)
	}
	var b strings.Builder
	p.Render(&b)
	text := b.String()
	for _, want := range []string{"admin@hub.example.com there", "ED25519 SHA256:", "authorize key", "2 shared, 2 skipped", "target skipped: local", "target skipped: w2"} {
		if !strings.Contains(text, want) {
			t.Fatalf("review lacks %q:\n%s", want, text)
		}
	}
	kept, _ := splitTargets(f, req, targets)
	if len(kept) != 2 || kept[0].ID != "laptop-2" || kept[0].SSHHost != "" || kept[1].ID != "w1" {
		t.Fatalf("kept %+v", kept)
	}

	req.NoAuthorize, req.Targets = true, false
	p, err = BuildHubShare(req, f, targets)
	if err != nil || ids(p) != "hub-host-key,import,verify" {
		t.Fatalf("--no-authorize: %v %s", err, ids(p))
	}
}

func TestHubShareRefusals(t *testing.T) {
	cases := map[string]func(*ShareFacts){
		"not joined":      func(f *ShareFacts) { f.Membership = hubshare.Membership{} },
		"another hub":     func(f *ShareFacts) { f.Membership.CAFingerprint = "bb" },
		"no lazylongarm":  func(f *ShareFacts) { f.Bin = "" },
		"has no `hub imp": func(f *ShareFacts) { f.HasImport = false },
		"no default SSH":  func(f *ShareFacts) { f.PubKey = nil },
		"nothing pinned":  func(f *ShareFacts) { f.KnownHosts = nil },
	}
	for want, mutate := range cases {
		req, f := shareFixture()
		mutate(f)
		if _, err := BuildHubShare(req, f, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", want, err)
		}
	}
	// A machine that already reaches the hub host needs no key.
	req, f := shareFixture()
	f.PubKey, f.CanSSH, f.HostKeyKnown = nil, true, true
	if _, err := BuildHubShare(req, f, nil); err != nil {
		t.Fatal(err)
	}
}

func TestHubImportRegisters(t *testing.T) {
	t.Setenv("LONGARM_CONFIG_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg, err := config.Load(path, false)
	if err != nil {
		t.Fatal(err)
	}
	payload := hubshare.Payload{Version: 1, Hub: config.Hub{ID: "h", Mode: "public", Address: "hub.example.com:7443", CAFingerprint: "aa", SSHHost: "hub-box"},
		Targets: []config.Target{{ID: "w1", SSHHost: "w1"}}}
	p, err := BuildHubImport(ImportRequest{Payload: payload, Source: "stdin", Config: cfg})
	if err != nil || ids(p) != "register" {
		t.Fatalf("%v %s", err, ids(p))
	}
	env := plan.NewEnv()
	if err := p.Steps[0].Apply(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	again, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if h, ok := again.Hub(""); !ok || h.SSHHost != "hub-box" || len(again.Targets) != 1 || again.Targets[0].Hub != "h" {
		t.Fatalf("saved %+v", again)
	}
	if !strings.Contains(env.Get("merge"), `"hub":"added"`) {
		t.Fatalf("merge %s", env.Get("merge"))
	}
	if err := p.Steps[0].Apply(context.Background(), env); err != nil || !strings.Contains(env.Get("merge"), `"hub":"unchanged"`) {
		t.Fatalf("idempotent: %v %s", err, env.Get("merge"))
	}
}

func TestPickIncludeFile(t *testing.T) {
	home := "/home/me"
	cases := []struct {
		pats []string
		want string
	}{
		{[]string{"~/.ssh/config.d/*"}, "/home/me/.ssh/config.d/lazylongarm-hub-h"},
		{[]string{"~/.ssh/config.d/*", "~/.ssh/lazylongarm/*.conf"}, "/home/me/.ssh/lazylongarm/lazylongarm-hub-h.conf"},
		{[]string{"config.d/*.conf"}, "/home/me/.ssh/config.d/lazylongarm-hub-h.conf"},
		{[]string{"~/.orbstack/ssh/config", "/etc/ssh/*/x"}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := PickIncludeFile(c.pats, home, "lazylongarm-hub-h"); got != c.want {
			t.Fatalf("%q: got %q want %q", c.pats, got, c.want)
		}
	}
}

func TestHubShareIdentity(t *testing.T) {
	req, f := shareFixture()
	req.Identity = "~/.ssh/work_ed25519"
	f.Alias, f.SSHHost, f.IdentityExists = "lazylongarm-hub-h", "lazylongarm-hub-h", true
	f.AliasFile = "/home/me/.ssh/config.d/lazylongarm-hub-h"
	p, err := BuildHubShare(req, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(p); got != "hub-ssh-alias,hub-host-key,hub-authorize,import,verify" {
		t.Fatalf("steps %s", got)
	}
	var b strings.Builder
	p.Render(&b)
	for _, want := range []string{"Host lazylongarm-hub-h", "HostName hub.example.com", "User admin", "IdentityFile ~/.ssh/work_ed25519", "IdentitiesOnly yes"} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("review lacks %q:\n%s", want, b.String())
		}
	}
	f.AliasFile = ""
	if _, err := BuildHubShare(req, f, nil); err == nil || !strings.Contains(err.Error(), "Include") {
		t.Fatalf("no include: %v", err)
	}
	f.AliasFile, f.IdentityExists = "/x", false
	if _, err := BuildHubShare(req, f, nil); err == nil || !strings.Contains(err.Error(), "no identity file") {
		t.Fatalf("missing identity: %v", err)
	}
}
