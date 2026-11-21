package companion

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testHome = "/home/me"

var testFile = filepath.Join(testHome, ".ssh", "lazylongarm", "longarm.conf")

func TestInjectRemoveIncludeRoundTrip(t *testing.T) {
	line := "Include ~/.ssh/lazylongarm/*.conf"
	for name, in := range map[string]string{
		"empty":        "",
		"host first":   "Host a\n  HostName a.example.com\n",
		"comment top":  "# Added by some tool: keep at the top\nInclude ~/.ssh/other/*.conf\n\nHost *\n  ServerAliveInterval 30\n",
		"blank first":  "\nHost a\n",
		"no final nl":  "Host a\n  User me",
		"crlf":         "Host a\r\n  User me\r\n",
		"only newline": "\n",
	} {
		t.Run(name, func(t *testing.T) {
			out, changed := InjectInclude([]byte(in), line)
			if !changed {
				t.Fatal("not changed")
			}
			if !strings.HasPrefix(string(out), IncludeBegin) {
				t.Fatalf("block not at the top:\n%s", out)
			}
			if !HasInclude(out, testFile, testHome) {
				t.Fatalf("HasInclude false:\n%s", out)
			}
			if strings.Contains(in, "\r\n") && strings.Count(string(out), "\n") != strings.Count(string(out), "\r\n") {
				t.Fatalf("mixed line endings: %q", out)
			}
			again, changed := InjectInclude(out, line)
			if changed || string(again) != string(out) {
				t.Fatalf("second inject changed it:\n%s", again)
			}
			back, removed := RemoveInclude(out)
			if !removed || string(back) != in {
				t.Fatalf("remove did not restore:\n%q\nwant\n%q", back, in)
			}
		})
	}
}

func TestInjectReplacesAChangedLineAndRemoveIgnoresOthers(t *testing.T) {
	in, _ := InjectInclude([]byte("Host a\n"), "Include ~/old.conf")
	out, changed := InjectInclude(in, "Include ~/new.conf")
	if !changed || strings.Contains(string(out), "old.conf") || !strings.Contains(string(out), "Include ~/new.conf") || strings.Count(string(out), IncludeBegin) != 1 {
		t.Fatalf("%s", out)
	}
	if _, removed := RemoveInclude([]byte("Include ~/.ssh/lazylongarm/*.conf\nHost a\n")); removed {
		t.Fatal("removed a line the user wrote")
	}
}

func TestHasInclude(t *testing.T) {
	for in, want := range map[string]bool{
		"# mine\ninclude   ~/.ssh/lazylongarm/longarm.conf\nHost a\n": true,  // the user's own line
		"Include ~/.ssh/lazylongarm/*.conf\n":                         true,  // a glob covering it
		"Include lazylongarm/longarm.conf\n":                          true,  // relative to ~/.ssh
		`Include "/home/me/.ssh/lazylongarm/longarm.conf"` + "\n":     true,  // quoted, absolute
		"Include ~/.ssh/dev.d/*.conf\n":                               false, // another directory
		"Host a\n  Include ~/.ssh/lazylongarm/*.conf\n":               false, // only applies to host a
		"# Include ~/.ssh/lazylongarm/*.conf\n":                       false, // a comment
		"":                                                            false,
	} {
		if got := HasInclude([]byte(in), testFile, testHome); got != want {
			t.Errorf("HasInclude(%q) = %v", in, got)
		}
	}
}

func TestIncludeLine(t *testing.T) {
	home := filepath.FromSlash("/home/me")
	if got := IncludeLine(filepath.Join(home, ".ssh", "lazylongarm", "longarm.conf"), home); got != "Include ~/.ssh/lazylongarm/*.conf" {
		t.Fatal(got)
	}
	if got := IncludeLine("/etc/ssh/my aliases.conf", home); got != `Include "/etc/ssh/my aliases.conf"` {
		t.Fatal(got)
	}
}

const genW1 = `Host longarm-worker-1
  HostName worker-1
  User longarm
  ProxyCommand /opt/bin/longarm --config-dir /home/me/.config/longarm proxy worker-1
  StrictHostKeyChecking yes

`

func TestMergeAliasesKeepsOtherWorkers(t *testing.T) {
	first := MergeAliases(nil, []byte(genW1))
	gen2 := strings.ReplaceAll(genW1, "worker-1", "worker-2")
	both := MergeAliases(first, []byte(gen2))
	if got := strings.Join(AliasHosts(both), ","); got != "longarm-worker-1,longarm-worker-2" {
		t.Fatalf("%s\n%s", got, both)
	}
	updated := MergeAliases(both, []byte(strings.ReplaceAll(genW1, "/opt/bin", "/usr/local/bin")))
	if strings.Count(string(updated), "Host longarm-worker-1\n") != 1 || !strings.Contains(string(updated), "/usr/local/bin/longarm") || strings.Contains(string(updated), "/opt/bin/longarm --config-dir /home/me/.config/longarm proxy worker-1") {
		t.Fatalf("%s", updated)
	}
}

func TestProxyCommandAndResolve(t *testing.T) {
	tools := Tools{Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "ssh" || strings.Join(args, " ") != "-G -- longarm-worker-1" {
			t.Fatalf("%s %v", name, args)
		}
		return []byte("user longarm\nhostname worker-1\nproxycommand /opt/bin/longarm --config-dir /x proxy worker-1\n"), nil
	}}
	p, err := tools.ProxyCommand(context.Background(), "longarm-worker-1")
	if err != nil || !ResolvesThroughLongarm(p, "worker-1") || ResolvesThroughLongarm(p, "worker-10") || ResolvesThroughLongarm("ssh -W %h:%p jump proxy worker-1", "worker-1") {
		t.Fatalf("%q %v", p, err)
	}
	if ValidWorkerName("worker-1") != nil || ValidWorkerName("a b") == nil || ValidWorkerName("-oProxyCommand=x") == nil {
		t.Fatal("ValidWorkerName")
	}
}

func TestWriteFileAtomicFollowsSymlinkAndKeepsMode(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.WriteFile(real, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(link, []byte("new"), 0o600, 0o700); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Lstat(link)
	data, _ := os.ReadFile(real)
	rst, _ := os.Stat(real)
	if st.Mode()&os.ModeSymlink == 0 || string(data) != "new" || rst.Mode().Perm() != 0o644 {
		t.Fatalf("link %v data %q mode %v", st.Mode(), data, rst.Mode())
	}
}

func TestFindLongarm(t *testing.T) {
	list := []Remote{
		{Name: "a", Target: "worker-1"},
		{Name: "b", Target: "longarm-worker-2"},
		{Name: "c", Target: "/usr/local/bin/longarm ssh worker-3 --"},
	}
	for w, want := range map[string]string{"worker-1": "", "worker-2": "b", "worker-3": "c", "worker-30": ""} {
		r, ok := FindLongarm(list, w)
		if ok != (want != "") || r.Name != want {
			t.Errorf("%s: %+v %v", w, r, ok)
		}
	}
	if !list[1].ViaLongarm() || !list[2].ViaLongarm() || list[0].ViaLongarm() {
		t.Fatal("ViaLongarm")
	}
}
