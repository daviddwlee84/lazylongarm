package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.1.0", "v0.1.0", 0}, {"v0.1.0", "v0.2.0", -1}, {"v1.10.0", "v1.9.9", 1},
		{"dev", "v0.1.0", -1}, {"v0.1.0", "garbage", 1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestExtractFindsTheBinaryOnly(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"LICENSE": "MIT", "lazylongarm": "BINARY", "completions/lazylongarm.bash": "x"} {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	got, err := extract(buf.Bytes(), "lazylongarm")
	if err != nil || string(got) != "BINARY" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := extract(buf.Bytes(), "missing"); err == nil {
		t.Fatal("missing file found")
	}
}

func TestInspectLeavesLocalBuildsAlone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9"}`))
	}))
	defer srv.Close()
	apiBase = srv.URL
	defer func() { apiBase = "https://api.github.com" }()
	c, err := Inspect(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	// The test binary is a local build: never self-replace it.
	if c.Owner != OwnerDev || c.Supported || c.Latest != "v9.9.9" || c.UpToDate {
		t.Fatalf("%+v", c)
	}
	if err := Apply(context.Background(), c, &bytes.Buffer{}); err == nil {
		t.Fatal("applied an upgrade to a local build")
	}
}

func fakeBrew(t *testing.T, installed string) *[]string {
	t.Helper()
	var calls []string
	old := execCmd
	t.Cleanup(func() { execCmd = old })
	execCmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		out := ""
		if strings.HasPrefix(call, "brew list --versions") {
			out = Formula + " " + installed
		}
		return exec.CommandContext(ctx, "sh", "-c", `printf '%s\n' "$1"`, "sh", out)
	}
	return &calls
}

func TestApplyHomebrewUpdatesAndChecksTheResult(t *testing.T) {
	calls := fakeBrew(t, "0.5.0")
	c := Check{Owner: OwnerHomebrew, Supported: true, Current: "v0.4.3", Latest: "v0.5.0"}
	if err := Apply(context.Background(), c, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := "brew update --quiet|brew upgrade " + Formula + "|brew list --versions " + Formula
	if got := strings.Join(*calls, "|"); got != want {
		t.Fatalf("calls\n got %s\nwant %s", got, want)
	}
}

func TestApplyHomebrewTapLagIsAnError(t *testing.T) {
	fakeBrew(t, "0.4.3") // brew upgrade only warned "already installed"
	c := Check{Owner: OwnerHomebrew, Supported: true, Current: "v0.4.3", Latest: "v0.5.0"}
	err := Apply(context.Background(), c, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "still installs v0.4.3") {
		t.Fatalf("want a tap-lag error, got %v", err)
	}
}
