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
	case "user-add":
		// upstream cmd/longarm/users.go printJoinCode + printPanel
		fmt.Println("K7Q2PX")
		fmt.Fprintln(os.Stderr, "admin certificate alice-laptop for user alice, code expires 11-22 10:00")
		fmt.Fprintln(os.Stderr, "on the user's machine: longarm join hub.example.com:7443 K7Q2PX --ca-fingerprint sha256:aa")
		fmt.Fprintln(os.Stderr, "web panel login alice (shown once): password Tr0ub4dor-3")
		fmt.Fprintln(os.Stderr, "TOTP secret JBSWY3DPEHPK3PXP")
		fmt.Fprintln(os.Stderr, "otpauth://totp/Longarm:alice?secret=JBSWY3DPEHPK3PXP")
	case "user-ls":
		// protojson with proto names (upstream printJSON), int32 as numbers
		fmt.Print(`{"users":[{"name":"alice","teams":["web"],"created_at":"2026-11-21T10:00:00Z","panel":true,"device_count":1,"worker_count":3},{"name":"bob","disabled":true}],"default_quota":{},"tenancy":true}`)
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

func TestAdminJoinCodeRoles(t *testing.T) {
	for _, tc := range []struct {
		req  JoinCodeRequest
		want []string
		not  []string
	}{
		{JoinCodeRequest{Name: "w1", TTL: 15 * time.Minute, Labels: []string{"gpu=none"}}, []string{"--role worker", "--name w1", "--labels gpu=none"}, []string{"--user"}},
		{JoinCodeRequest{Role: "client", Name: "w1-client", User: "alice", TTL: 15 * time.Minute, Labels: []string{"gpu=none"}}, []string{"--role client", "--name w1-client", "--user alice"}, []string{"--labels"}},
	} {
		argv := strings.Join(joinCodeArgs(tc.req), " ")
		for _, w := range tc.want {
			if !strings.Contains(argv, w) {
				t.Errorf("%+v: argv %q lacks %q", tc.req, argv, w)
			}
		}
		for _, n := range tc.not {
			if strings.Contains(argv, n) {
				t.Errorf("%+v: argv %q has %q", tc.req, argv, n)
			}
		}
	}
}

func TestAdminUserAddParsesCodeAndPanel(t *testing.T) {
	c := fakeLongarm(t, "user-add")
	r, err := c.AdminUserAdd(context.Background(), UserAddRequest{Name: "alice", Device: "alice-laptop", Admin: true, Panel: true, TTL: 15 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if r.Code.Reveal() != "K7Q2PX" || r.Password.Reveal() != "Tr0ub4dor-3" || r.TOTP.Reveal() != "JBSWY3DPEHPK3PXP" || !strings.HasPrefix(r.Otpauth.Reveal(), "otpauth://totp/") {
		t.Fatalf("parsed %q %q %q %q", r.Code.Reveal(), r.Password.Reveal(), r.TOTP.Reveal(), r.Otpauth.Reveal())
	}
	if b, _ := json.Marshal(r); strings.Contains(string(b), "K7Q2PX") || strings.Contains(string(b), "Tr0ub4dor") {
		t.Fatal("secrets leak through JSON")
	}
	argv := strings.Join(userAddArgs(UserAddRequest{Name: "alice", Device: "d1", Admin: true, Panel: true, TTL: time.Hour, Quota: []string{"--max-jobs=8"}}), " ")
	if argv != "admin user add alice --device d1 --admin --panel --ttl 1h0m0s --max-jobs=8" {
		t.Fatalf("argv %q", argv)
	}
	if argv := strings.Join(userAddArgs(UserAddRequest{Name: "bob", NoCode: true, TTL: time.Hour}), " "); argv != "admin user add bob --no-code" {
		t.Fatalf("no-code argv %q", argv)
	}
	if _, err := parseUserAdded("added bob\n", "", false); err == nil {
		t.Fatal("\"added bob\" taken for a code")
	}
}

func TestAdminUsersDecodes(t *testing.T) {
	c := fakeLongarm(t, "user-ls")
	l, err := c.AdminUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a, ok := l.Find("alice")
	if !l.Tenancy || !ok || a.DeviceCount != 1 || a.WorkerCount != 3 || !a.Panel || a.Teams[0] != "web" {
		t.Fatalf("users %+v", l)
	}
	if b, _ := l.Find("bob"); !b.Disabled {
		t.Fatalf("bob %+v", b)
	}
}

func TestJoinCodeOwnerOnlyForWorkers(t *testing.T) {
	argv := strings.Join(joinCodeArgs(JoinCodeRequest{Name: "w1", Owner: "alice", TTL: 15 * time.Minute}), " ")
	if !strings.Contains(argv, "--role worker") || !strings.Contains(argv, "--owner alice") {
		t.Fatalf("argv %q", argv)
	}
	if argv := strings.Join(joinCodeArgs(JoinCodeRequest{Role: "client", Name: "c1", Owner: "alice", TTL: time.Minute}), " "); strings.Contains(argv, "--owner") {
		t.Fatalf("client code with --owner: %q", argv)
	}
}
