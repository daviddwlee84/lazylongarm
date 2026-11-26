package cli

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestJobRequestArgs(t *testing.T) {
	line := `echo "$HOME" && grep 'a b' f`
	cases := []struct {
		req  jobRequest
		want []string
	}{
		{jobRequest{Worker: "worker-1", Command: line, Name: "check", Cwd: "src"},
			[]string{"run", "--on", "worker-1", "--name", "check", "--cwd", "src", "--", "sh", "-c", line}},
		{jobRequest{Command: "nvidia-smi", Require: []string{"gpu=t4", "os=linux"}},
			[]string{"run", "--require", "gpu=t4", "--require", "os=linux", "--", "sh", "-c", "nvidia-smi"}},
		{jobRequest{Worker: "auto", Exec: true, Name: "ignored", Require: []string{"x=y"}, Timeout: 5 * time.Minute, Argv: []string{"uname", "-a"}},
			[]string{"exec", "--timeout", "5m0s", "--", "uname", "-a"}},
		{jobRequest{Worker: "win-1", Windows: true, Command: "Get-Date"},
			[]string{"run", "--on", "win-1", "--", "powershell", "-NoProfile", "-Command", "Get-Date"}},
	}
	for _, c := range cases {
		if got := c.req.longarmArgs(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%+v:\n got %q\nwant %q", c.req, got, c.want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"plain":        "plain",
		"os=linux":     "os=linux",
		"":             "''",
		"a b":          "'a b'",
		`it's`:         `'it'\''s'`,
		`echo "$HOME"`: `'echo "$HOME"'`,
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestJobReviewShowsExactCommandAndWarnings(t *testing.T) {
	r := jobRequest{Command: "echo 'hi'\nuname -a"}
	text := r.reviewText(true)
	for _, want := range []string{
		`longarm run -- sh -c 'echo '\''hi'\''`,
		"  │ echo 'hi'\n  │ uname -a\n",
		"for this display only",
		"auto may pick a Windows worker",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("review lacks %q:\n%s", want, text)
		}
	}
	if text := (jobRequest{Worker: "worker-1", Command: "ls"}).reviewText(true); strings.Contains(text, "Windows") || strings.Contains(text, "!") {
		t.Errorf("unexpected warnings:\n%s", text)
	}
}

func TestJobRunUsage(t *testing.T) {
	for _, args := range [][]string{
		{"job", "run"}, // no command, no terminal
		{"job", "run", "w1", "w2"},
		{"job", "run", "w1", "-c", "ls", "--", "uname"},
	} {
		if code, _, errOut := run(t, false, args...); code != 2 {
			t.Errorf("%q: exit %d, %s", args, code, errOut)
		}
	}
}
