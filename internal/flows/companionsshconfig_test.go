package flows

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daviddwlee84/lazylongarm/internal/companion"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
)

// fakeSSH resolves longarm-<w> through longarm only when ~/.ssh/config
// includes the alias file and the file has the host, like OpenSSH would.
func fakeSSH(t *testing.T, req SSHConfigRequest, chezmoi bool) companion.Tools {
	t.Helper()
	return companion.Tools{
		LookPath: func(name string) (string, error) {
			if name == "chezmoi" && !chezmoi {
				return "", fmt.Errorf("not found")
			}
			return "/bin/" + name, nil
		},
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			switch name {
			case "chezmoi":
				return []byte("/src/private_dot_ssh/config\n"), nil
			case "ssh":
				host := args[len(args)-1]
				cfg, _ := os.ReadFile(req.UserConfig)
				aliases, _ := os.ReadFile(req.File)
				if companion.HasInclude(cfg, req.File, req.Home) && strings.Contains(string(aliases), "Host "+host+"\n") {
					w := strings.TrimPrefix(host, companion.LongarmHostPrefix)
					return []byte("proxycommand /opt/longarm --config-dir /c proxy " + w + "\n"), nil
				}
				return []byte("proxycommand none\n"), nil
			}
			return nil, fmt.Errorf("unexpected %s", name)
		},
	}
}

func sshReq(t *testing.T, workers ...string) SSHConfigRequest {
	home := t.TempDir()
	return SSHConfigRequest{Workers: workers, File: companion.DefaultAliasFile(home), UserConfig: companion.UserSSHConfig(home), Home: home}
}

func fakeLongarm(calls *[]string) func(context.Context, ...string) ([]byte, error) {
	return func(_ context.Context, args ...string) ([]byte, error) {
		*calls = append(*calls, strings.Join(args, " "))
		var b strings.Builder
		for _, w := range args[1:] {
			fmt.Fprintf(&b, "Host longarm-%s\n  HostName %s\n  ProxyCommand /opt/longarm --config-dir /c proxy %s\n\n", w, w, w)
		}
		return []byte(b.String()), nil
	}
}

func TestSSHConfigAppliesIncludesAndRemoves(t *testing.T) {
	req := sshReq(t, "worker-1", "worker-2")
	orig := "# mine\nHost a\n  User me\n"
	if err := os.MkdirAll(filepath.Dir(req.UserConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(req.UserConfig, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	tools := fakeSSH(t, req, false)
	var calls []string
	deps := SSHConfigDeps{Tools: tools, Longarm: fakeLongarm(&calls)}
	st, err := ReadSSHConfigState(context.Background(), req, tools)
	if err != nil || st.Resolves["worker-1"] || st.IncludePresent || st.ChezmoiManaged {
		t.Fatalf("%+v %v", st, err)
	}
	p, err := BuildCompanionSSHConfig(req, st, deps)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(p); got != "ssh-aliases,ssh-include,ssh-resolve" {
		t.Fatalf("steps %s", got)
	}
	results, err := (&plan.Runner{}).Run(context.Background(), p, plan.NewEnv(), p.Digest())
	if err != nil {
		t.Fatalf("%v %+v", err, results)
	}
	if strings.Join(calls, ";") != "ssh-config worker-1 worker-2" {
		t.Fatalf("longarm calls %v", calls)
	}
	cfg, _ := os.ReadFile(req.UserConfig)
	bak, _ := os.ReadFile(req.UserConfig + ".lazylongarm.bak")
	if !strings.HasPrefix(string(cfg), companion.IncludeBegin+"\nInclude ~/.ssh/lazylongarm/*.conf\n") || string(bak) != orig {
		t.Fatalf("config:\n%s\nbackup:\n%s", cfg, bak)
	}
	if st, _ := os.Stat(req.File); st.Mode().Perm() != 0o600 {
		t.Fatalf("alias file mode %v", st.Mode())
	}

	// Rerun: nothing to do.
	st, _ = ReadSSHConfigState(context.Background(), req, tools)
	if p, _ := BuildCompanionSSHConfig(req, st, deps); len(p.Steps) != 0 {
		t.Fatalf("rerun steps %s", ids(p))
	}

	// A new worker is merged; the first ones stay.
	req3 := req
	req3.Workers = []string{"worker-3"}
	st, _ = ReadSSHConfigState(context.Background(), req3, tools)
	p, _ = BuildCompanionSSHConfig(req3, st, deps)
	if got := ids(p); got != "ssh-aliases,ssh-resolve" {
		t.Fatalf("steps %s", got)
	}
	if _, err := (&plan.Runner{}).Run(context.Background(), p, plan.NewEnv(), p.Digest()); err != nil {
		t.Fatal(err)
	}
	aliases, _ := os.ReadFile(req.File)
	if got := strings.Join(companion.AliasHosts(aliases), ","); got != "longarm-worker-1,longarm-worker-2,longarm-worker-3" {
		t.Fatal(got)
	}

	// Remove restores ~/.ssh/config byte for byte.
	rp := BuildCompanionSSHConfigRemove(req)
	if _, err := (&plan.Runner{}).Run(context.Background(), rp, plan.NewEnv(), rp.Digest()); err != nil {
		t.Fatal(err)
	}
	cfg, _ = os.ReadFile(req.UserConfig)
	if string(cfg) != orig {
		t.Fatalf("not restored:\n%q", cfg)
	}
	if _, err := os.Stat(filepath.Dir(req.File)); !os.IsNotExist(err) {
		t.Fatalf("alias dir left: %v", err)
	}
	results, err = (&plan.Runner{}).Run(context.Background(), rp, plan.NewEnv(), rp.Digest())
	if err != nil || results[0].Status != plan.Skipped || results[1].Status != plan.Skipped {
		t.Fatalf("second remove: %v %+v", err, results)
	}
}

func TestSSHConfigLeavesChezmoiAndNoIncludeAlone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		include string
		chezmoi bool
		warn    string
	}{
		{"chezmoi auto", "", true, "chezmoi manages"},
		{"no", IncludeNo, false, "--include no"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := sshReq(t, "worker-1")
			req.Include = tc.include
			tools := fakeSSH(t, req, tc.chezmoi)
			var calls []string
			st, _ := ReadSSHConfigState(context.Background(), req, tools)
			p, err := BuildCompanionSSHConfig(req, st, SSHConfigDeps{Tools: tools, Longarm: fakeLongarm(&calls)})
			if err != nil {
				t.Fatal(err)
			}
			if got := ids(p); got != "ssh-aliases,ssh-resolve" {
				t.Fatalf("steps %s", got)
			}
			if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], tc.warn) || !strings.Contains(p.Warnings[0], "Include ~/.ssh/lazylongarm/*.conf") {
				t.Fatalf("warnings %v", p.Warnings)
			}
			results, err := (&plan.Runner{}).Run(context.Background(), p, plan.NewEnv(), p.Digest())
			if err == nil || results[1].Status != plan.Failed || !strings.Contains(results[1].Fix, "--include yes") {
				t.Fatalf("resolve must fail with a fix: %v %+v", err, results)
			}
			if _, err := os.Stat(req.UserConfig); !os.IsNotExist(err) {
				t.Fatal("~/.ssh/config was written")
			}
		})
	}
	req := sshReq(t, "worker-1")
	req.Include = "sometimes"
	if _, err := BuildCompanionSSHConfig(req, SSHConfigState{}, SSHConfigDeps{}); err == nil {
		t.Fatal("bad --include accepted")
	}
	req.Include, req.Workers = "", []string{"-oProxyCommand=x"}
	if _, err := BuildCompanionSSHConfig(req, SSHConfigState{}, SSHConfigDeps{}); err == nil {
		t.Fatal("bad worker name accepted")
	}
}

func TestCompanionRemotesThroughLongarm(t *testing.T) {
	lists := map[string][]companion.Remote{
		companion.HC: {
			{Name: "worker-1", Target: "worker-1", Enabled: true},                   // over its SSH host
			{Name: "worker-3-la", Target: "longarm ssh worker-3 --", Enabled: true}, // hc --via: already through longarm
		},
		companion.Herdr: {{ID: "abc", Name: "worker-1", Target: "worker-1", Enabled: true}},
	}
	targets := []RemoteTarget{{Name: "worker-1", SSHHost: "worker-1", Worker: "worker-1"}, {Name: "worker-2", Worker: "worker-2"}, {Name: "worker-3", Worker: "worker-3"}}
	req := RemotesRequest{Targets: targets, Transport: TransportLongarm, SSH: sshReq(t)}
	st := SSHConfigState{Resolves: map[string]bool{"worker-1": true, "worker-2": true, "worker-3": true}}
	p, err := BuildCompanionRemotes(req, RemotesDeps{Lists: lists, SSHState: st})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(p); got != "hc-worker-2,herdr-worker-2,herdr-worker-3" {
		t.Fatalf("steps %s", got)
	}
	var b strings.Builder
	p.Render(&b)
	for _, s := range []string{"hc machine add longarm-worker-2 --install --name worker-2", "herdr machine add longarm-worker-3 --label worker-3", "already registered: worker-3", "worker-1 (as worker-1)", "--switch"} {
		if !strings.Contains(b.String(), s) {
			t.Errorf("review lacks %q:\n%s", s, b.String())
		}
	}

	req.Switch = true
	p, err = BuildCompanionRemotes(req, RemotesDeps{Lists: lists, SSHState: st})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(p); got != "hc-forget-worker-1,hc-worker-1,hc-worker-2,herdr-forget-worker-1,herdr-worker-1,herdr-worker-2,herdr-worker-3" {
		t.Fatalf("switch steps %s", got)
	}
	b.Reset()
	p.Render(&b)
	for _, s := range []string{"hc machine rm worker-1", "herdr machine remove abc", "hc machine add longarm-worker-1 --install --name worker-1"} {
		if !strings.Contains(b.String(), s) {
			t.Errorf("review lacks %q:\n%s", s, b.String())
		}
	}

	// Aliases not resolved yet: the ssh-config steps come first.
	p, _ = BuildCompanionRemotes(RemotesRequest{Targets: targets[1:2], Transport: TransportLongarm, Tools: []string{companion.HC}, SSH: sshReq(t)}, RemotesDeps{Lists: lists})
	if got := ids(p); got != "ssh-aliases,ssh-include,ssh-resolve,hc-worker-2" {
		t.Fatalf("steps %s", got)
	}
	if _, err := BuildCompanionRemotes(RemotesRequest{Targets: []RemoteTarget{{Name: "x"}}, Transport: TransportLongarm}, RemotesDeps{}); err == nil {
		t.Fatal("target without worker accepted")
	}
}
