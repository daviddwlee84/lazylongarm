package longarm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string, v any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "upstream", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}

func TestUpstreamFixturesDecode(t *testing.T) {
	var v VersionInfo
	fixture(t, "version.json", &v)
	if v.Version == "" || len(v.Features) == 0 {
		t.Fatalf("version: %+v", v)
	}
	var w WhoAmI
	fixture(t, "whoami.json", &w)
	if w.Name != "alice" || w.Role != "client" {
		t.Fatalf("whoami: %+v", w)
	}
	var wl WorkerList
	fixture(t, "worker-ls.json", &wl)
	if len(wl.Nodes) == 0 {
		t.Fatal("no nodes")
	}
	n := wl.Nodes[0]
	if n.Metrics == nil || n.Metrics.MemTotalBytes == 0 || n.MemPercent() <= 0 {
		t.Fatalf("metrics int64-as-string not decoded: %+v", n.Metrics)
	}
	if got := n.Path(); !strings.Contains(got, "relay") || !strings.Contains(got, "direct") {
		t.Fatalf("path = %q", got)
	}
	var jl JobList
	fixture(t, "jobs.json", &jl)
	if len(jl.Jobs) < 2 {
		t.Fatal("jobs")
	}
	if jl.Jobs[0].Job.ShortState() != "running" || !jl.Jobs[0].Job.Active() {
		t.Fatalf("state: %s", jl.Jobs[0].Job.State)
	}
	if jl.Jobs[1].Job.ShortState() != "killed" || jl.Jobs[1].Job.ExitCode != 137 {
		t.Fatalf("killed job: %+v", jl.Jobs[1].Job)
	}
	var d Doctor
	fixture(t, "doctor.json", &d)
	if d.Schema != 1 || len(d.Checks) == 0 || d.Checks[3].Fix == "" {
		t.Fatalf("doctor: %+v", d)
	}
}

func TestKindForExitCodes(t *testing.T) {
	cases := map[int]Kind{2: KindBadRequest, 3: KindNotFound, 69: KindHubDown, 75: KindRefused, 77: KindNotAllowed, 137: KindKilled, 254: KindLost, 255: KindUnknown, 1: KindUnknown}
	for code, want := range cases {
		if got := kindFor(code); got != want {
			t.Errorf("kindFor(%d) = %s, want %s", code, got, want)
		}
	}
}

func TestRedactArgsHidesJoinCode(t *testing.T) {
	got := redactArgs([]string{"join", "hub.example.com:7443", "SECRETCODE", "--ca-fingerprint", "sha256:x"})
	if strings.Contains(got, "SECRETCODE") || !strings.Contains(got, "<code>") {
		t.Fatalf("got %q", got)
	}
}

// fakeLongarm runs this test binary as `longarm`, recording argv.
func fakeLongarm(t *testing.T, mode string) *Client {
	t.Helper()
	t.Setenv("LONGARM_HELPER", mode) // Client.Run builds Env from os.Environ()
	commandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestHelperLongarm", "--"}, args...)...)
		return cmd
	}
	t.Cleanup(func() { commandContext = exec.CommandContext })
	return &Client{Bin: "/fake/longarm"}
}

func TestHelperLongarm(t *testing.T) {
	mode := os.Getenv("LONGARM_HELPER")
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
	switch mode {
	case "version":
		fmt.Print(`{"version":"v0.1.42","features":["job-kill-if-created-at"]}`)
	case "echo":
		if args[0] == "version" {
			fmt.Print(`{"version":"v0.1.42","features":["job-kill-if-created-at"]}`)
			os.Exit(0)
		}
		fmt.Print(strings.Join(args, " "))
	case "worker-add":
		fmt.Println("ABCD-EFGH-1234")
		fmt.Fprintln(os.Stderr, "run longarmd join ...")
	case "denied":
		fmt.Fprintln(os.Stderr, "certificate expired")
		os.Exit(77)
	}
	os.Exit(0)
}

func TestWorkerAddReturnsSecret(t *testing.T) {
	c := fakeLongarm(t, "worker-add")
	code, err := c.WorkerAdd(context.Background(), "w1", 15*time.Minute, []string{"gpu=none"})
	if err != nil {
		t.Fatal(err)
	}
	if code.Reveal() != "ABCD-EFGH-1234" || code.String() != "<redacted>" {
		t.Fatalf("code %q / %s", code.Reveal(), code)
	}
	b, _ := json.Marshal(code)
	if strings.Contains(string(b), "ABCD") {
		t.Fatal("secret leaked through JSON")
	}
}

func TestExitCodeMapsToKind(t *testing.T) {
	c := fakeLongarm(t, "denied")
	_, err := c.WhoAmI(context.Background())
	if KindOf(err) != KindNotAllowed {
		t.Fatalf("err = %v (kind %s)", err, KindOf(err))
	}
}

func TestKillUsesIfCreatedAtWhenAdvertised(t *testing.T) {
	c := fakeLongarm(t, "echo")
	out, err := c.Run(context.Background(), 0, nil, "kill", "w1", "J1")
	if err != nil || string(out) != "kill w1 J1" {
		t.Fatalf("%q %v", out, err)
	}
	if !c.Has(context.Background(), "job-kill-if-created-at") {
		t.Fatal("feature not detected")
	}
}
