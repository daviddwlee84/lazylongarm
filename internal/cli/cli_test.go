package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, tty bool, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("LAZYLONGARM_CONFIG", "")
	var out, errb bytes.Buffer
	deps := Dependencies{
		Terminal: func(io.Reader, io.Writer) bool { return tty },
		RunTUI:   func(context.Context, *options) error { out.WriteString("<dashboard>"); return nil },
	}
	code := execute(context.Background(), args, strings.NewReader(""), &out, &errb, deps)
	return code, out.String(), errb.String()
}

func TestEntryPolicy(t *testing.T) {
	if code, out, _ := run(t, false); code != 0 || !strings.Contains(out, "Usage:") {
		t.Fatalf("non-TTY bare should print help: %d %q", code, out)
	}
	if code, out, _ := run(t, true); code != 0 || out != "<dashboard>" {
		t.Fatalf("TTY bare should open the dashboard: %d %q", code, out)
	}
	code, _, errOut := run(t, true, "--json")
	if code != 2 || !strings.Contains(errOut, `"code":"usage"`) {
		t.Fatalf("bare --json: %d %s", code, errOut)
	}
	if code, _, errOut := run(t, false, "hub", "setup"); code != 2 || !strings.Contains(errOut, "--host and --address") {
		t.Fatalf("non-TTY wizard: %d %s", code, errOut)
	}
	if code, _, _ := run(t, false, "hub", "setup", "--host", "box"); code != 2 {
		t.Fatalf("partial flags must be a usage error, got %d", code)
	}
	code, _, errOut = run(t, true, "hub", "setup", "--adress", "x", "--json")
	if code != 2 || !strings.Contains(errOut, `"usage"`) {
		t.Fatalf("typo flag must not start a wizard: %d %s", code, errOut)
	}
	if code, out, _ := run(t, false, "hub"); code != 0 || !strings.Contains(out, "setup") {
		t.Fatalf("group shows help: %d %q", code, out)
	}
	if code, _, errOut := run(t, false, "worker", "add"); code != 2 || !strings.Contains(errOut, "no hub configured") {
		t.Fatalf("worker add without hub: %d %s", code, errOut)
	}
	if code, _, errOut := run(t, false, "peer", "add"); code != 2 || !strings.Contains(errOut, "no hub configured") {
		t.Fatalf("peer add without hub: %d %s", code, errOut)
	}
}

func TestStatusJSONWithoutHub(t *testing.T) {
	code, out, _ := run(t, false, "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var rep map[string]any
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if !strings.HasSuffix(rep["config"].(string), filepath.Join("lazylongarm", "config.toml")) {
		t.Fatalf("config path %v", rep["config"])
	}
}

// A config written by a newer lazylongarm: commands still run, stdout stays
// valid JSON, and one warning on stderr names the keys and the fix.
func TestUnknownConfigKeysWarnOnStderr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	data := "[[targets]]\nid = \"worker-1\"\npool_name = \"c1\"\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, false, "--config", path, "settings", "show", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var rep struct {
		Unknown []string `json:"unknown_keys"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if len(rep.Unknown) != 1 || rep.Unknown[0] != "targets[worker-1].pool_name" {
		t.Fatalf("unknown_keys %q", rep.Unknown)
	}
	if strings.Count(errOut, "! ") != 1 || !strings.Contains(errOut, "targets[worker-1].pool_name") || !strings.Contains(errOut, "lazylongarm upgrade") {
		t.Fatalf("warning: %q", errOut)
	}
	// A type error is still fatal and names the line.
	if err := os.WriteFile(path, []byte("[[targets]]\nid = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := run(t, false, "--config", path, "settings", "show"); code != 1 || !strings.Contains(errOut, "line 2") {
		t.Fatalf("type error: %d %s", code, errOut)
	}
}

func TestSkillPrints(t *testing.T) {
	code, out, _ := run(t, false, "--skill")
	if code != 0 || !strings.Contains(out, "name: lazylongarm") {
		t.Fatalf("%d %q", code, out[:min(len(out), 80)])
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

// hub panel opens a tailnet URL only when this machine is on that tailnet;
// otherwise it says why and goes for the SSH tunnel.
func TestHubPanelTailnetRoute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	data := `default_hub = "hub-1"
[[hubs]]
id = "hub-1"
mode = "public"
address = "hub.example.com:7443"
ssh_host = "hub-host.invalid"
[hubs.panel_access]
mode = "tailnet"
url = "https://hub-1.tail1234.ts.net/"
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	old := tailnetCheck
	defer func() { tailnetCheck = old }()

	tailnetCheck = func(context.Context, string) (bool, string) { return true, "" }
	code, out, errOut := run(t, false, "--config", path, "hub", "panel", "--no-browser")
	if code != 0 || !strings.Contains(out, "panel: https://hub-1.tail1234.ts.net/") {
		t.Fatalf("on tailnet: exit %d\n%s\n%s", code, out, errOut)
	}

	tailnetCheck = func(context.Context, string) (bool, string) {
		return false, "Tailscale is not installed on this machine"
	}
	_, out, errOut = run(t, false, "--config", path, "hub", "panel", "--no-browser")
	if strings.Contains(out, "ts.net") || !strings.Contains(errOut, "not installed on this machine: using the SSH tunnel instead") {
		t.Fatalf("off tailnet:\n%s\n%s", out, errOut)
	}
}
