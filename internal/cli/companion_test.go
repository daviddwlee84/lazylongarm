package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daviddwlee84/lazylongarm/internal/companion"
)

func runWith(t *testing.T, cfg string, tools companion.Tools, args ...string) (int, string, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := filepath.Join(dir, "config.toml")
	if cfg != "" {
		if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LAZYLONGARM_CONFIG", path)
	var out, errb bytes.Buffer
	deps := Dependencies{Terminal: func(io.Reader, io.Writer) bool { return false }, Companion: tools}
	code := execute(context.Background(), args, strings.NewReader(""), &out, &errb, deps)
	return code, out.String(), errb.String()
}

const fleetCfg = `
[[targets]]
id = "worker-1"
name = "worker-1"
ssh_host = "worker-1"

[[targets]]
id = "worker-2"
name = "worker-2"
ssh_host = "me@worker-2.example.com"
dotfiles_bundle = "server"
`

func fakeTools(hc string) companion.Tools {
	return companion.Tools{
		LookPath: func(name string) (string, error) {
			if name == companion.HC {
				return "/bin/hc", nil
			}
			return "", errors.New("not found")
		},
		Run: func(context.Context, string, ...string) ([]byte, error) { return []byte(hc), nil },
	}
}

func TestCompanionStatusNoRemote(t *testing.T) {
	code, out, errOut := runWith(t, fleetCfg, fakeTools(`{"machines":[{"name":"worker-1","target":"worker-1","state":"connected"}]}`), "companion", "status", "--no-remote", "--json")
	if code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	var rep struct {
		Machines []companionRow `json:"machines"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Machines) != 2 || rep.Machines[0].Remotes["hc"] != "connected" || rep.Machines[1].Remotes["hc"] != "missing" || rep.Machines[0].Remotes["herdr"] != "not installed" || rep.Machines[0].Dotfiles != nil {
		t.Fatalf("%+v", rep.Machines)
	}
}

func TestCompanionRemotesDryRunAndTargets(t *testing.T) {
	tools := fakeTools(`{"machines":[{"name":"worker-1","target":"worker-1"}]}`)
	code, out, errOut := runWith(t, fleetCfg, tools, "companion", "remotes", "--all", "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	if !strings.Contains(out, `hc machine add me@worker-2.example.com --install --name worker-2`) || strings.Contains(out, "add worker-1 ") {
		t.Fatalf("plan: %s", out)
	}
	if code, _, errOut := runWith(t, fleetCfg, tools, "companion", "remotes"); code != 2 || !strings.Contains(errOut, "--all") {
		t.Fatalf("non-TTY without machines: %d %s", code, errOut)
	}
	if code, _, errOut := runWith(t, fleetCfg, tools, "companion", "remotes", "worker-1", "--all"); code != 2 || !strings.Contains(errOut, "not both") {
		t.Fatalf("args + --all: %d %s", code, errOut)
	}
	if code, out, _ := runWith(t, fleetCfg, tools, "companion", "remotes", "worker-1", "--yes"); code != 0 || !strings.Contains(out, "Nothing to register") {
		t.Fatalf("already registered: %d %q", code, out)
	}
}

func TestCompanionDotfilesNeedsRepoWhenNotInTerminal(t *testing.T) {
	code, _, errOut := runWith(t, fleetCfg, fakeTools(`{"machines":[]}`), "companion", "dotfiles")
	if code != 2 || !strings.Contains(errOut, "--all") {
		t.Fatalf("%d %s", code, errOut)
	}
}

func TestCompanionStatusSurvivesAListError(t *testing.T) {
	tools := fakeTools("")
	tools.Run = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("socket path too long")
	}
	code, out, errOut := runWith(t, fleetCfg, tools, "companion", "status", "--no-remote", "--json")
	if code != 0 || !strings.Contains(out, `"hc": "error"`) || !strings.Contains(out, "socket path too long") {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	if code, _, errOut := runWith(t, fleetCfg, tools, "companion", "remotes", "--all", "--tool", "hc", "--dry-run"); code != 1 || !strings.Contains(errOut, "socket path too long") {
		t.Fatalf("remotes must fail on an unreadable list: %d %s", code, errOut)
	}
}

func TestCompanionSSHConfigAndLongarmRemotesDryRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tools := companion.Tools{
		LookPath: func(name string) (string, error) {
			if name == "chezmoi" || name == companion.Herdr {
				return "", errors.New("not found")
			}
			return "/bin/" + name, nil
		},
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			switch name {
			case "ssh":
				return []byte("proxycommand none\n"), nil
			case companion.HC:
				return []byte(`{"machines":[{"name":"worker-1","target":"worker-1"}]}`), nil
			}
			return nil, errors.New("unexpected " + name)
		},
	}
	code, out, errOut := runWith(t, fleetCfg, tools, "companion", "ssh-config", "worker-1", "friend-box", "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	for _, s := range []string{"longarm ssh-config worker-1 friend-box", filepath.Join(home, ".ssh", "lazylongarm", "longarm.conf"), "Include ~/.ssh/lazylongarm/*.conf"} {
		if !strings.Contains(out, s) {
			t.Errorf("plan lacks %q: %s", s, out)
		}
	}
	if code, _, errOut := runWith(t, fleetCfg, tools, "companion", "ssh-config", "worker-1", "--include", "maybe", "--dry-run"); code != 2 || !strings.Contains(errOut, "--include") {
		t.Fatalf("bad --include: %d %s", code, errOut)
	}
	if code, _, errOut := runWith(t, fleetCfg, tools, "companion", "ssh-config", "worker-1", "--remove"); code != 2 || !strings.Contains(errOut, "no workers") {
		t.Fatalf("--remove with workers: %d %s", code, errOut)
	}
	if code, _, errOut := runWith(t, fleetCfg, tools, "companion", "ssh-config", "--remove", "--yes"); code != 0 {
		t.Fatalf("--remove on a clean HOME: %d %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh", "config")); !os.IsNotExist(err) {
		t.Fatalf("--remove created ~/.ssh/config: %v", err)
	}

	code, out, errOut = runWith(t, fleetCfg, tools, "companion", "remotes", "--all", "--transport", "longarm", "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	for _, s := range []string{"ssh-aliases", "hc machine add longarm-worker-2 --install --name worker-2", "worker-1 (as worker-1)"} {
		if !strings.Contains(out, s) {
			t.Errorf("remotes plan lacks %q: %s", s, out)
		}
	}
	if code, _, errOut := runWith(t, fleetCfg, tools, "companion", "remotes", "--all", "--switch", "--dry-run"); code != 2 || !strings.Contains(errOut, "--transport longarm") {
		t.Fatalf("--switch without longarm: %d %s", code, errOut)
	}
}

func TestCompanionStatusShowsLongarmEntries(t *testing.T) {
	tools := fakeTools(`{"machines":[{"name":"worker-1","target":"worker-1","state":"connected"},{"name":"w1-la","target":"longarm-worker-1","state":"connected"},{"name":"w2","target":"longarm ssh worker-2 --","state":"connecting"}]}`)
	code, out, errOut := runWith(t, fleetCfg, tools, "companion", "status", "--no-remote", "--json")
	if code != 0 || !strings.Contains(out, `"hc": "connected +longarm"`) || !strings.Contains(out, `"hc": "connecting (longarm)"`) {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
}

func TestCompanionFerryUsageErrors(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"companion", "ferry", "pair", "worker-1", "worker-2:/in"}, "write <machine>:<path>"},
		{[]string{"companion", "ferry", "pair", "worker-1:/data", "worker-2:/in", "--via", "rsync"}, "unknown relay adapter"},
		{[]string{"companion", "ferry", "pair", "worker-1:/data", "worker-2:/in", "--via", "rclone"}, "needs --remote"},
		{[]string{"companion", "ferry", "pair", "worker-1:/data", "worker-2:/in", "--via", "ghrelease"}, "needs --repo"},
		{[]string{"companion", "ferry", "pair", "worker-1:/data", "worker-2:/in", "--remote", "gdrive:"}, "--remote is for --via rclone"},
		{[]string{"companion", "ferry", "pair", "worker-1:/data", "worker-1:/in"}, "same machine"},
		{[]string{"companion", "ferry", "setup", "worker-1", "--backend", "dropbox"}, "unknown ferry backend"},
		{[]string{"companion", "ferry", "setup"}, "when not in a terminal"},
	} {
		code, _, errOut := runWith(t, fleetCfg, fakeTools(`{"machines":[]}`), tc.args...)
		if code != 2 || !strings.Contains(errOut, tc.want) {
			t.Errorf("%v: exit %d, stderr %q; want 2 and %q", tc.args, code, errOut, tc.want)
		}
	}
}
