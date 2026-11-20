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

func TestSkillPrints(t *testing.T) {
	code, out, _ := run(t, false, "--skill")
	if code != 0 || !strings.Contains(out, "name: lazylongarm") {
		t.Fatalf("%d %q", code, out[:min(len(out), 80)])
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
