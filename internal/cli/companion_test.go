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
