package connection

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// fakeSSH replaces ssh with this test binary; argv is echoed to stderr for
// assertions and mode selects the behavior.
func fakeSSH(t *testing.T, mode string) *[]string {
	t.Helper()
	var seen []string
	commandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		seen = append(seen, name+" "+strings.Join(args, " "))
		cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestHelperSSH", "--", name}, args...)...)
		cmd.Env = append(os.Environ(), "FAKE_SSH="+mode)
		return cmd
	}
	t.Cleanup(func() { commandContext = exec.CommandContext; clearMultiplexPolicies() })
	return &seen
}

func TestHelperSSH(t *testing.T) {
	mode := os.Getenv("FAKE_SSH")
	if mode == "" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) > 1 && args[1] == "-G" {
		fmt.Println("hostname box.example.com\ncontrolmaster false\ncontrolpersist no")
		os.Exit(0)
	}
	switch mode {
	case "ok":
		fmt.Print("hello")
	case "auth":
		fmt.Fprintln(os.Stderr, "Permission denied (publickey).")
		os.Exit(255)
	case "timeout":
		fmt.Fprintln(os.Stderr, "ssh: connect to host box port 22: Operation timed out")
		os.Exit(255)
	case "sudo":
		fmt.Fprintln(os.Stderr, "sudo: a password is required")
		os.Exit(1)
	case "fail":
		fmt.Fprintln(os.Stderr, "\x1b[31mboom\x1b[0m")
		os.Exit(3)
	}
	os.Exit(0)
}

func TestRunRemoteUsesBatchModeAndQuotesScript(t *testing.T) {
	seen := fakeSSH(t, "ok")
	res, err := Run(context.Background(), Request{Host: "box", Script: `echo "$1"`, Args: []string{"it's"}})
	if err != nil || string(res.Stdout) != "hello" {
		t.Fatalf("%q %v", res.Stdout, err)
	}
	last := (*seen)[len(*seen)-1]
	for _, want := range []string{"BatchMode=yes", "StrictHostKeyChecking=yes", "ClearAllForwardings=yes", "ForwardAgent=no", "-T -- box", `'sh' '-c' 'echo "$1"' 'lazylongarm' 'it'"'"'s'`} {
		if !strings.Contains(last, want) {
			t.Errorf("missing %q in %s", want, last)
		}
	}
}

func TestRunClassifiesFailures(t *testing.T) {
	fakeSSH(t, "auth")
	if _, err := Run(context.Background(), Request{Host: "box", Script: "true"}); !IsAuthRequired(err) {
		t.Fatalf("auth: %v", err)
	}
	fakeSSH(t, "timeout")
	var te *SSHTransportError
	if _, err := Run(context.Background(), Request{Host: "box", Script: "true"}); !errors.As(err, &te) || te.Kind != "timeout" {
		t.Fatalf("timeout: %v", err)
	}
	fakeSSH(t, "sudo")
	if _, err := Run(context.Background(), Request{Host: "box", Script: "true", Sudo: true}); !errors.Is(err, ErrSudoPasswordRequired) {
		t.Fatalf("sudo: %v", err)
	}
	fakeSSH(t, "fail")
	var ce *CommandError
	_, err := Run(context.Background(), Request{Host: "box", Script: "false"})
	if !errors.As(err, &ce) || ce.ExitCode != 3 || strings.Contains(ce.Stderr, "\x1b") || !strings.Contains(ce.Stderr, "boom") {
		t.Fatalf("fail: %#v", err)
	}
}

func TestSudoBatchUsesNonInteractiveFlag(t *testing.T) {
	seen := fakeSSH(t, "ok")
	if _, err := Run(context.Background(), Request{Host: "box", Script: "id", Sudo: true}); err != nil {
		t.Fatal(err)
	}
	if last := (*seen)[len(*seen)-1]; !strings.Contains(last, "'sudo' '-n' '--' 'sh'") {
		t.Fatalf("sudo argv: %s", last)
	}
	cmd, err := InteractiveCommand(context.Background(), Request{Host: "box", Script: "id", Sudo: true})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "-tt") || !strings.Contains(joined, "BatchMode=no") || strings.Contains(joined, "'-n'") {
		t.Fatalf("interactive argv: %s", joined)
	}
}

func TestValidateHost(t *testing.T) {
	for _, ok := range []string{"my-vps", "admin@10.0.0.1", "ts_nas", "box.example.com"} {
		if err := ValidateHost(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-oProxyCommand=x", "a b", "x;rm", "$(id)"} {
		if err := ValidateHost(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSanitizeStripsEscapes(t *testing.T) {
	if got := Sanitize("a\x1b]0;title\x07b\x1b[1mc\x00"); got != "abc" {
		t.Fatalf("%q", got)
	}
}
