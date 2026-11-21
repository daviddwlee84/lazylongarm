package connection

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func TestRunStallStopsASilentCommand(t *testing.T) {
	fakeSSH(t, "stall")
	start := time.Now()
	_, err := Run(context.Background(), Request{Host: "box", Script: "cat", Stall: 300 * time.Millisecond, Timeout: time.Minute})
	var stall *StallError
	if !errors.As(err, &stall) || stall.Stall != 300*time.Millisecond {
		t.Fatalf("want a StallError, got %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("stall detected after %s", took)
	}
}

func TestRunStallToleratesSlowProgress(t *testing.T) {
	fakeSSH(t, "trickle") // ~480 ms of output, a line every 60 ms
	res, err := Run(context.Background(), Request{Host: "box", Script: "cat", Stall: 250 * time.Millisecond, Timeout: time.Minute})
	if err != nil {
		t.Fatalf("slow but progressing command failed: %v", err)
	}
	if !strings.Contains(string(res.Stdout), "lazylongarm-progress 800") {
		t.Fatalf("output %q", res.Stdout)
	}
}

func TestPushFileReportsProgressAndStalls(t *testing.T) {
	fakeSSH(t, "trickle")
	var seen []int64
	err := PushFile(context.Background(), "box", []byte("0123456789"), "$HOME/.local/bin/x", "0755", PushOptions{Progress: func(n, total int64) {
		if total != 10 {
			t.Errorf("total %d", total)
		}
		seen = append(seen, n)
	}})
	if err != nil || len(seen) != 8 || seen[7] != 800 {
		t.Fatalf("progress %v, err %v", seen, err)
	}

	fakeSSH(t, "stall")
	old := PushStall
	PushStall = 300 * time.Millisecond
	t.Cleanup(func() { PushStall = old })
	err = PushFile(context.Background(), "box", []byte("data"), "$HOME/.local/bin/x", "0755", PushOptions{})
	if err == nil || !strings.Contains(err.Error(), "stalled") || !strings.Contains(err.Error(), "nothing was installed") {
		t.Fatalf("stalled push: %v", err)
	}
}

func TestPushTimeoutScalesWithSize(t *testing.T) {
	if got := PushTimeout(0); got != 2*time.Minute {
		t.Fatalf("empty: %s", got)
	}
	// 40 MB at 16 KiB/s is about 40 minutes, plus two.
	if got := PushTimeout(40 << 20); got < 40*time.Minute || got > 45*time.Minute {
		t.Fatalf("40 MiB: %s", got)
	}
}

func TestPushFileLocalVerifiesBeforeRename(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "bin", "longarmd")
	data := []byte("binary contents\n")
	if err := PushFile(context.Background(), "", data, dest, "0755", PushOptions{SHA256: sum(data)}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dest)
	if err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("installed: %v %v", st, err)
	}
	if got, _ := os.ReadFile(dest); string(got) != string(data) {
		t.Fatalf("content %q", got)
	}

	// A wrong checksum (or a short copy) leaves the installed file alone.
	err = PushFile(context.Background(), "", []byte("other"), dest, "0755", PushOptions{SHA256: sum(data)})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("mismatch: %v", err)
	}
	if got, _ := os.ReadFile(dest); string(got) != string(data) {
		t.Fatalf("mismatch replaced the file: %q", got)
	}
	assertNoTemp(t, filepath.Dir(dest))
}

// fakeCurl puts a curl on PATH that serves files from dir by URL basename,
// or fails like curl does, per FAKE_CURL (ok | fail | bad).
func fakeCurl(t *testing.T, mode string) string {
	t.Helper()
	bin, assets := t.TempDir(), t.TempDir()
	script := `#!/bin/sh
out=; url=
while [ $# -gt 0 ]; do
  case "$1" in -o) out="$2"; shift;; https://*) url="$1";; esac
  shift
done
case "$FAKE_CURL" in
  fail) echo "curl: (7) Failed to connect to github.com port 443" >&2; exit 7;;
  bad) printf 'tampered' > "$out";;
  *) cat "$FAKE_ASSETS/${url##*/}" > "$out";;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CURL", mode)
	t.Setenv("FAKE_ASSETS", assets)
	return assets
}

func TestFetchFileLocal(t *testing.T) {
	opts := FetchOptions{ConnectTimeout: 5 * time.Second, SpeedLimit: 1024, SpeedTime: 10 * time.Second, Timeout: time.Minute}
	data := []byte("released binary\n")
	url := "https://github.com/example/longarm/releases/download/v1.2.3/longarm-linux-amd64"

	assets := fakeCurl(t, "ok")
	if err := os.WriteFile(filepath.Join(assets, "longarm-linux-amd64"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "bin", "longarm")
	if err := FetchFile(context.Background(), "", url, sum(data), dest, "0755", opts); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dest); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("installed: %v %v", st, err)
	}

	t.Setenv("FAKE_CURL", "bad")
	other := filepath.Join(filepath.Dir(dest), "longarmd")
	err := FetchFile(context.Background(), "", url, sum(data), other, "0755", opts)
	var fe *FetchError
	if !errors.As(err, &fe) || fe.Kind != FetchChecksum {
		t.Fatalf("mismatch: %v", err)
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatal("a mismatching download was installed")
	}

	t.Setenv("FAKE_CURL", "fail")
	err = FetchFile(context.Background(), "", url, sum(data), other, "0755", opts)
	if !errors.As(err, &fe) || fe.Kind != FetchDownload || !strings.Contains(fe.Detail, "Failed to connect") {
		t.Fatalf("download failure: %v", err)
	}
	assertNoTemp(t, filepath.Dir(dest))

	if err := FetchFile(context.Background(), "", "http://example.com/x", sum(data), other, "0755", opts); err == nil {
		t.Fatal("plain http accepted")
	}
	if err := FetchFile(context.Background(), "", url, "abc", other, "0755", opts); err == nil {
		t.Fatal("bad sha256 accepted")
	}
}

func TestFetchCommandShowsBounds(t *testing.T) {
	got := FetchCommand("https://github.com/o/r/releases/download/v1/a", "/home/u/.local/bin/a", "0755", FetchOptions{ConnectTimeout: 10 * time.Second, SpeedLimit: 32768, SpeedTime: 20 * time.Second})
	for _, want := range []string{"--connect-timeout 10", "--speed-limit 32768 --speed-time 20", "-o /home/u/.local/bin/a.lazylongarm.$$ https://github.com/o/r/releases/download/v1/a", "wget --tries=1", "rename to /home/u/.local/bin/a", "install nothing"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".lazylongarm.") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}
