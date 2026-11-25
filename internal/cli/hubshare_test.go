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

func runStdin(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("LONGARM_CONFIG_DIR", t.TempDir())
	var out, errb bytes.Buffer
	deps := Dependencies{Terminal: func(io.Reader, io.Writer) bool { return false }}
	code := execute(context.Background(), args, strings.NewReader(stdin), &out, &errb, deps)
	return code, out.String(), errb.String()
}

func TestHubImportStdin(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("LAZYLONGARM_CONFIG", "")
	payload := `version = 1
from = "laptop-1"

[hub]
id = "hub-1"
mode = "public"
address = "hub.example.com:7443"
ca_fingerprint = "sha256:AA"
ssh_host = "admin@hub.example.com"

[[targets]]
id = "w1"
ssh_host = "w1"
`
	code, out, errOut := runStdin(t, payload, "hub", "import", "--stdin", "--yes", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var res struct {
		Merge struct {
			HubID   string `json:"hub_id"`
			Hub     string `json:"hub"`
			Default bool   `json:"default"`
		} `json:"merge"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Merge.HubID != "hub-1" || res.Merge.Hub != "added" || !res.Merge.Default {
		t.Fatalf("%v %+v\n%s", err, res, out)
	}
	b, _ := os.ReadFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "lazylongarm", "config.toml"))
	if !strings.Contains(string(b), `ssh_host = 'admin@hub.example.com'`) || !strings.Contains(string(b), `hub = 'hub-1'`) {
		t.Fatalf("config:\n%s", b)
	}
	// Again: unchanged.
	code, out, _ = runStdin(t, payload, "hub", "import", "--stdin", "--yes", "--json")
	if code != 0 || !strings.Contains(out, `"hub": "unchanged"`) {
		t.Fatalf("again %d %s", code, out)
	}
	if code, _, errOut := runStdin(t, payload, "hub", "import", "--stdin", "--ssh-host", "x", "--yes"); code != 2 || !strings.Contains(errOut, "only --id") {
		t.Fatalf("--stdin with --ssh-host: %d %s", code, errOut)
	}
	if code, _, errOut := runStdin(t, "garbage = 1", "hub", "import", "--stdin", "--yes"); code != 2 || !strings.Contains(errOut, "not a hub payload") {
		t.Fatalf("garbage: %d %s", code, errOut)
	}
}

func TestHubImportFromLongarmNotJoined(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("LAZYLONGARM_CONFIG", "")
	code, _, errOut := runStdin(t, "", "hub", "import", "--dry-run")
	if code != 2 || !strings.Contains(errOut, "not joined") {
		t.Fatalf("%d %s", code, errOut)
	}
}

func TestHubShareNeedsHub(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("LAZYLONGARM_CONFIG", "")
	code, _, errOut := runStdin(t, "", "hub", "share", "box")
	if code != 2 || !strings.Contains(errOut, "hub import") {
		t.Fatalf("%d %s", code, errOut)
	}
}
