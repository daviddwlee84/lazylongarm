package companion

import (
	"os/exec"
	"strings"
	"testing"
)

func TestParseFerryState(t *testing.T) {
	out := "ferry=/home/u/.local/bin/ferry\nferry_version=0.2.0\nuv=/home/u/.local/bin/uv\n" +
		"baidupcs=/home/u/.local/bin/BaiduPCS-Go\nbaidupcs_login=yes\nrclone=/usr/bin/rclone\nrclone_remotes=gdrive: s3: \ngh=\ndone=yes\n"
	s, err := ParseFerryState(out)
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != "0.2.0" || !s.SignedIn(BackendBaiduPCS) || !s.HasRcloneRemote("s3:bucket/relay") || s.HasRcloneRemote("box:") {
		t.Fatalf("%+v", s)
	}
	if got := s.BackendState(BackendGH); got != "not-installed" {
		t.Fatalf("gh %s", got)
	}
	if got := s.Summary(); got != "0.2.0 · baidupcs ok · rclone ok" {
		t.Fatalf("summary %q", got)
	}
	if got := s.SignedInBackends(); strings.Join(got, ",") != "baidupcs,rclone" {
		t.Fatalf("signed in %v", got)
	}
	if _, err := ParseFerryState("ferry=\n"); err == nil {
		t.Fatal("truncated report accepted")
	}
	s, _ = ParseFerryState("ferry=\nbaidupcs=/x\nbaidupcs_login=no\ndone=yes\n")
	if s.Summary() != "not installed" || s.BackendState(BackendBaiduPCS) != "login-needed" || s.BackendState(BackendRclone) != "not-installed" {
		t.Fatalf("%+v %q", s, s.Summary())
	}
}

func TestFerryStateScriptRunsLocally(t *testing.T) {
	out, err := exec.Command("/bin/sh", "-c", FerryStateScript).Output()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := ParseFerryState(string(out)); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestExpandPathAndJobNames(t *testing.T) {
	for in, want := range map[string]string{"~": "/home/u", "~/data/x/": "/home/u/data/x", "data": "/home/u/data", "/srv//in/": "/srv/in"} {
		if got, err := ExpandPath(in, "/home/u"); err != nil || got != want {
			t.Errorf("ExpandPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ExpandPath("~bob/x", "/home/u"); err == nil {
		t.Error("~user accepted")
	}
	if got := JobNameFrom("/data/My Photos 2024/"); got != "My-Photos-2024" {
		t.Errorf("JobNameFrom %q", got)
	}
	if got := JobNameFrom("/"); got != "transfer" {
		t.Errorf("JobNameFrom(/) %q", got)
	}
	if ValidJobName("-x") == nil || ValidJobName("a b") == nil || ValidJobName("run42.v2") != nil {
		t.Error("ValidJobName")
	}
	if JobPath("/home/u", "x") != "/home/u/.config/ferry/jobs/x.toml" {
		t.Error("JobPath")
	}
}

func TestFerryJobRender(t *testing.T) {
	j := FerryJob{
		Name: "run42", SenderName: "worker-1", ReceiverName: "worker-2", Root: "/data/run42", Glob: "*",
		SourceSelected: "each entry in the directory is one unit", Dest: "/srv/in \"quoted\"", Adapter: "rclone",
		Prefix: "/ferry/run42", Options: map[string]string{"remote": "gdrive:"}, GeneratedBy: "lazylongarm v1",
	}
	got, err := j.Render()
	if err != nil {
		t.Fatal(err)
	}
	want := `# ferry job written by lazylongarm v1 (companion ferry pair).
# The same file is on both machines: sender worker-1, receiver worker-2.
# Backend logins live in each machine's backend CLI, never here.
name = "run42"

[units]
root = "/data/run42"
glob = "*"  # each entry in the directory is one unit

[sender]
delete_source = "never"

[receiver]
dest = "/srv/in \"quoted\""
extract = false

[remote]
adapter = "rclone"
prefix = "/ferry/run42"
egress = "direct"
options = { remote = "gdrive:" }
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	for _, bad := range []FerryJob{
		{Name: "x", Root: "rel", Glob: "*", Dest: "/d", Adapter: "baidupcs"},
		{Name: "x", Root: "/r", Glob: "../*", Dest: "/d", Adapter: "baidupcs"},
		{Name: "x", Root: "/r", Glob: "*", Dest: "/d", Adapter: "scp"},
		{Name: "x", Root: "/r", Glob: "*", Dest: "/d", Adapter: "baidupcs", DeleteSource: "always"},
	} {
		if _, err := bad.Render(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestJobFileScriptNeverReplaces(t *testing.T) {
	p := t.TempDir() + "/jobs/x.toml"
	run := func(mode, body string) string {
		t.Helper()
		cmd := exec.Command("/bin/sh", "-c", JobFileScript, "sh", p, mode)
		cmd.Stdin = strings.NewReader(body)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%v %s", err, out)
		}
		st, err := ParseJobFileState(string(out))
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	steps := [][3]string{
		{"check", "a\n", "missing"},
		{"write", "a\n", "written"},
		{"check", "a\n", "same"},
		{"write", "a\n", "same"},
		{"write", "b\n", "differs"},
	}
	for _, s := range steps {
		if got := run(s[0], s[1]); got != s[2] {
			t.Fatalf("%s %q: %s, want %s", s[0], s[1], got, s[2])
		}
	}
	if out, _ := exec.Command("cat", p).Output(); string(out) != "a\n" {
		t.Fatalf("file replaced: %q", out)
	}
}

func TestBaiduPCSAssetAndAdapters(t *testing.T) {
	if got := BaiduPCSAsset("v4.0.2", "darwin", "arm64"); got != "BaiduPCS-Go-v4.0.2-darwin-osx-arm64" {
		t.Error(got)
	}
	if BaiduPCSAsset("v4.0.2", "windows", "amd64") != "" {
		t.Error("windows asset")
	}
	if b, err := AdapterBackend("ghrelease"); err != nil || b != BackendGH {
		t.Error(b, err)
	}
	if b, err := AdapterBackend("rsync"); err != nil || b != BackendRsync {
		t.Error(b, err)
	}
	if _, err := AdapterBackend("scp"); err == nil {
		t.Error("scp accepted")
	}
	if n, err := FerryPlanUnits([]byte(`{"units":[{"key":"a"},{"key":"b"}]}`)); err != nil || n != 2 {
		t.Error(n, err)
	}
}

func TestVersionAtLeast(t *testing.T) {
	for _, tc := range []struct {
		v, min string
		want   bool
	}{
		{"0.2.0", "0.2.0", true}, {"0.10.0", "0.2.0", true}, {"v0.3", "0.2.0", true},
		{"0.1.0", "0.2.0", false}, {"0.2.0rc1", "0.2.0", true}, {"", "0.2.0", false},
	} {
		if got := VersionAtLeast(tc.v, tc.min); got != tc.want {
			t.Errorf("VersionAtLeast(%q, %q) = %v", tc.v, tc.min, got)
		}
	}
	if !strings.Contains(DefaultFerrySpec, "@") {
		t.Error("DefaultFerrySpec is not pinned")
	}
}

func TestRsyncJobAndRoute(t *testing.T) {
	j := FerryJob{
		Name: "run42", Root: "/data/run42", Glob: "*", Dest: "/srv/in", Adapter: "rsync", Prefix: "/ferry/run42",
		Options:         map[string]string{"host": "nas", "root": "/home/u/.cache/ferry/run42/landing"},
		ReceiverAdapter: "local", ReceiverOptions: map[string]string{"root": "/home/u/.cache/ferry/run42/landing"},
	}
	got, err := j.Render()
	if err != nil {
		t.Fatal(err)
	}
	want := `options = { host = "nas", root = "/home/u/.cache/ferry/run42/landing" }

[remote.sides.receiver]
adapter = "local"
options = { root = "/home/u/.cache/ferry/run42/landing" }
`
	if !strings.HasSuffix(got, want) {
		t.Fatalf("got:\n%s", got)
	}
	r := ParseSSHRoute("hostname 100.64.0.7\nproxycommand none\nproxyjump none\n")
	if r.HostName != "100.64.0.7" || r.ProxyCommand != "" || r.ThroughLongarm("nas") {
		t.Fatalf("%+v", r)
	}
	r = ParseSSHRoute("hostname worker-2\nproxycommand /usr/local/bin/longarm proxy worker-2\n")
	if !r.ThroughLongarm("w2") || !(SSHRoute{}).ThroughLongarm("longarm-w2") {
		t.Fatalf("%+v", r)
	}
	if s, _ := ParseFerryState("ferry=/f\nrsync=/usr/bin/rsync\ndone=yes\n"); !s.Installed(BackendRsync) || !s.SignedIn(BackendRsync) {
		t.Fatalf("%+v", s)
	}
}
