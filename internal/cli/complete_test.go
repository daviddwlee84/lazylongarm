package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daviddwlee84/lazylongarm/internal/companion"
)

const completionCfg = `
default_hub = "hub-a"

[[hubs]]
id = "hub-a"
mode = "public"
address = "hub.example.com"

[[hubs]]
id = "hub-b"
name = "Lab hub"
mode = "private"
address = "100.64.0.1"

[[targets]]
id = "local"
name = "local"

[[targets]]
id = "worker-1"
name = "worker-1"
ssh_host = "worker-1"

[[targets]]
id = "worker-2"
name = "worker-2"
ssh_host = "me@worker-2.example.com"
worker_name = "gpu-2"
`

// complete runs `lazylongarm __complete args...` and returns the candidate
// lines (without descriptions) and the directive line.
func complete(t *testing.T, args ...string) ([]string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	sshCfg := "Host build-box\n  HostName build-box.example.com\nHost *.example.com\n  User me\nHost worker-1 worker-9\n"
	if err := os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte(sshCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runWith(t, completionCfg, companion.Tools{}, append([]string{"__complete"}, args...)...)
	if code != 0 {
		t.Fatalf("__complete %v: exit %d: %s", args, code, errOut)
	}
	var got []string
	directive := ""
	for line := range strings.Lines(out) {
		line = strings.TrimRight(line, "\n")
		if strings.HasPrefix(line, ":") {
			directive = line
			continue
		}
		name, _, _ := strings.Cut(line, "\t")
		got = append(got, name)
	}
	return got, directive
}

func TestCompletion(t *testing.T) {
	const noFile = ":4"
	for _, tc := range []struct {
		args      []string
		want      []string
		directive string
	}{
		{[]string{"worker", "add", ""}, []string{"build-box", "worker-1", "worker-9"}, noFile},
		{[]string{"peer", "add", ""}, []string{"build-box", "worker-1", "worker-9"}, noFile},
		{[]string{"worker", "add", "build-box", ""}, nil, noFile},
		{[]string{"hub", "setup", "--host", ""}, []string{"build-box", "worker-1", "worker-9"}, noFile},
		{[]string{"hub", "status", "--hub", ""}, []string{"hub-a", "hub-b"}, noFile},
		{[]string{"companion", "dotfiles", ""}, []string{"worker-1", "worker-2"}, noFile},
		{[]string{"companion", "dotfiles", "worker-1", ""}, []string{"worker-2"}, noFile},
		{[]string{"companion", "ssh-config", ""}, []string{"worker-1", "gpu-2"}, noFile},
		{[]string{"companion", "remotes", "--transport", ""}, []string{"ssh", "longarm"}, noFile},
		{[]string{"companion", "ferry", "setup", ""}, []string{"worker-1", "worker-2"}, noFile},
		{[]string{"companion", "ferry", "setup", "--backend", ""}, []string{"baidupcs", "rclone", "gh"}, noFile},
		{[]string{"companion", "ferry", "pair", ""}, []string{"worker-1:", "worker-2:"}, ":6"},
		{[]string{"companion", "ferry", "pair", "worker-1:/data", ""}, []string{"worker-1:", "worker-2:"}, ":6"},
		{[]string{"companion", "ferry", "pair", "worker-1:/data", "gpu-2:"}, nil, noFile},
		{[]string{"companion", "ferry", "pair", "--via", ""}, []string{"baidupcs", "rclone", "ghrelease", "rsync"}, noFile},
		{[]string{"skill", "print", ""}, []string{"skill"}, noFile},
		{[]string{"--color", ""}, []string{"auto", "always", "never"}, noFile},
		{[]string{"version", ""}, nil, noFile},
		{[]string{"user", "invite", ""}, nil, noFile},
		{[]string{"hub", "panel-access", ""}, []string{"loopback", "tailnet", "public"}, noFile},
	} {
		got, directive := complete(t, tc.args...)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") || directive != tc.directive {
			t.Errorf("__complete %q = %q %s, want %q %s", tc.args, got, directive, tc.want, tc.directive)
		}
	}
	// Path flags keep the shell's file completion.
	if _, directive := complete(t, "hub", "backup", "--out", ""); directive != ":0" {
		t.Errorf("--out should complete files, got %s", directive)
	}
}

// A broken config gives no candidates and no warning (stderr carries only
// cobra's own directive note, which shells discard).
func TestCompletionBrokenConfig(t *testing.T) {
	code, out, errOut := runWith(t, "not = [valid", companion.Tools{}, "__complete", "hub", "status", "--hub", "")
	if code != 0 || strings.Contains(errOut, "!") || strings.Count(strings.TrimSpace(errOut), "\n") > 0 || strings.TrimSpace(out) != ":4" {
		t.Fatalf("broken config: %d %q %q", code, out, errOut)
	}
}
