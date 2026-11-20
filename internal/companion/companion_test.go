package companion

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const hcJSON = `{"machines":[
 {"host":"box","id":"1","name":"worker_1","session":null,"state":"connected","target":"worker-1","temporary":false,"version":"0.1.17","via":false},
 {"host":"nas","id":"2","name":"nas","session":null,"state":"offline","target":"me@nas.example.com","temporary":false,"version":"0.1.16","via":false}]}`

const herdrJSON = `[{"id":"a","label":"worker-2","target":"worker-2","session":"default","enabled":true,"selected":false},
 {"id":"b","label":"old","target":"old-box","session":"default","enabled":false,"selected":false}]`

func TestParseAndFind(t *testing.T) {
	hc, err := ParseHC([]byte(hcJSON))
	if err != nil || len(hc) != 2 || hc[0].State != "connected" || !hc[0].Enabled {
		t.Fatalf("hc: %v %+v", err, hc)
	}
	herdr, err := ParseHerdr([]byte(herdrJSON))
	if err != nil || len(herdr) != 2 || herdr[1].Enabled || herdr[1].State != "disabled" {
		t.Fatalf("herdr: %v %+v", err, herdr)
	}
	if r, ok := Find(hc, "worker-1", "x"); !ok || r.Name != "worker_1" {
		t.Fatalf("by target: %v %+v", ok, r)
	}
	if r, ok := Find(herdr, "ssh-alias-unknown", "worker-2"); !ok || r.Target != "worker-2" {
		t.Fatalf("by name: %v %+v", ok, r)
	}
	if _, ok := Find(hc, "worker-9", "worker-9"); ok {
		t.Fatal("false match")
	}
	if _, err := ParseHC([]byte("not json")); err == nil {
		t.Fatal("bad json accepted")
	}
}

func TestToolsList(t *testing.T) {
	tools := Tools{
		LookPath: func(name string) (string, error) {
			if name == HC {
				return "/bin/hc", nil
			}
			return "", errors.New("not found")
		},
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name != HC || strings.Join(args, " ") != "machine ls --json" {
				t.Fatalf("unexpected %s %v", name, args)
			}
			return []byte(hcJSON), nil
		},
	}
	if !tools.Available(HC) || tools.Available(Herdr) {
		t.Fatal("availability")
	}
	list, err := tools.List(context.Background(), HC)
	if err != nil || len(list) != 2 {
		t.Fatalf("%v %v", err, list)
	}
}

func TestAddArgv(t *testing.T) {
	if got := strings.Join(AddArgv(HC, "worker-1", "w1"), " "); got != "hc machine add worker-1 --install --name w1" {
		t.Fatal(got)
	}
	if got := strings.Join(AddArgv(Herdr, "worker-1", "w1"), " "); got != "herdr machine add worker-1 --label w1" {
		t.Fatal(got)
	}
}

func TestParseStateStripsCredentials(t *testing.T) {
	s, err := ParseState("chezmoi=/home/u/.local/bin/chezmoi\nsource=/home/u/.local/share/chezmoi\norigin=https://user:ghp_secret@github.com/you/dotfiles.git\nrev=abc1234\ndone=yes\n")
	if err != nil || !s.Initialized() || s.Rev != "abc1234" {
		t.Fatalf("%v %+v", err, s)
	}
	if strings.Contains(s.Origin, "ghp_secret") || s.Origin != "https://github.com/you/dotfiles.git" {
		t.Fatalf("origin %q", s.Origin)
	}
	empty, err := ParseState("chezmoi=\ndone=yes\n")
	if err != nil || empty.Initialized() {
		t.Fatalf("%v %+v", err, empty)
	}
	if _, err := ParseState("chezmoi=\n"); err == nil {
		t.Fatal("incomplete output accepted")
	}
}

func TestSameRepo(t *testing.T) {
	same := [][2]string{
		{"https://github.com/you/dotfiles.git", "git@github.com:you/dotfiles.git"},
		{"you", "https://github.com/you/dotfiles"},
		{"you/dots", "ssh://git@github.com/you/dots.git"},
		{"https://GitHub.com/you/dotfiles/", "github.com/you/dotfiles"},
		{"https://gitlab.example.com/group/sub/dots.git", "git@gitlab.example.com:group/sub/dots"},
	}
	for _, c := range same {
		if !SameRepo(c[0], c[1]) {
			t.Errorf("%q vs %q: %q != %q", c[0], c[1], NormalizeRepo(c[0]), NormalizeRepo(c[1]))
		}
	}
	if SameRepo("you/dotfiles", "someone/dotfiles") || SameRepo("", "") {
		t.Error("different repos match")
	}
}

func TestBundleOrder(t *testing.T) {
	byOS := map[string]string{"darwin": "laptop", "linux": "server"}
	cases := []struct{ flag, target, os, want string }{
		{"cli", "rec", "linux", "cli"},
		{"", "rec", "linux", "rec"},
		{"", "", "darwin", "laptop"},
		{"", "", "freebsd", "fallback"},
	}
	for _, c := range cases {
		if got := Bundle(c.flag, c.target, byOS, c.os, "fallback"); got != c.want {
			t.Errorf("%+v: got %q", c, got)
		}
	}
}

func TestRenderQuotesAndRejectsTypos(t *testing.T) {
	tmpl := `curl -fsSL https://example.com/bootstrap.sh | bash -s -- --yes --bundle {bundle} --name {name}; echo "${HOME}"`
	got, err := Render(tmpl, map[string]string{"bundle": "server", "name": "x'; rm -rf / #"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "--bundle 'server'") || !strings.Contains(got, `--name 'x'"'"'; rm -rf / #'`) || !strings.Contains(got, `"${HOME}"`) {
		t.Fatalf("render: %s", got)
	}
	if _, err := Render("run {bundel}", map[string]string{"bundle": "x"}); err == nil || !strings.Contains(err.Error(), "unknown placeholder {bundel}") {
		t.Fatalf("typo: %v", err)
	}
	if _, err := Render("run --bundle {bundle}", map[string]string{}); err == nil || !strings.Contains(err.Error(), "no value") {
		t.Fatalf("empty: %v", err)
	}
	if keys := strings.Join(TemplateKeys("{repo} {os}{arch} ${name}"), ","); keys != "arch,os,repo" {
		t.Fatalf("keys %s", keys)
	}
}

func TestDefaultBootstrapScript(t *testing.T) {
	s, err := BootstrapScript("", map[string]string{"repo": "https://github.com/you/dotfiles.git"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{RemotePath, "get.chezmoi.io", "init --apply --promptDefaults 'https://github.com/you/dotfiles.git'"} {
		if !strings.Contains(s, want) {
			t.Errorf("default script lacks %q:\n%s", want, s)
		}
	}
	if _, err := BootstrapScript("", nil); err == nil {
		t.Fatal("default bootstrap without repo accepted")
	}
}
