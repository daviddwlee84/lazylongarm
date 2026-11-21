package flows

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/hostprobe"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/secret"
)

// fakeMachine makes "worker-1" a local directory: a fake ssh on PATH runs the
// remote command with the local sh (stdin passes through), and a fake curl
// serves the release assets or fails per FAKE_CURL (ok | fail | bad).
type fakeMachine struct {
	t       *testing.T
	facts   *hostprobe.Facts
	assets  map[string][]byte // asset name → contents
	cache   string            // this machine's verified release cache
	curlLog string

	mu      sync.Mutex
	fetched []string // assets the controller downloaded into its cache
}

func newFakeMachine(t *testing.T, curl string) *fakeMachine {
	t.Helper()
	bin, served, cache := t.TempDir(), t.TempDir(), t.TempDir()
	m := &fakeMachine{t: t, facts: ubuntu(), cache: cache, curlLog: filepath.Join(bin, "curl.log"), assets: map[string][]byte{
		"longarmd-linux-amd64": []byte("#!/bin/sh\necho longarmd v0.1.42\n"),
		"longarm-linux-amd64":  []byte("#!/bin/sh\necho longarm v0.1.42\n"),
	}}
	m.facts.Home = t.TempDir()
	for name, data := range m.assets {
		for _, dir := range []string{served, cache} {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	scripts := map[string]string{
		"ssh": `#!/bin/sh
case " $* " in *" -G "*) printf 'hostname worker-1.example.com\ncontrolmaster false\ncontrolpersist no\n'; exit 0;; esac
for last; do :; done
exec sh -c "$last"
`,
		"curl": `#!/bin/sh
out=; url=
while [ $# -gt 0 ]; do
  case "$1" in -o) out="$2"; shift;; https://*) url="$1";; esac
  shift
done
printf '%s\n' "$url" >> "$FAKE_CURL_LOG"
case "$FAKE_CURL" in
  fail) echo "curl: (7) Failed to connect to github.com port 443" >&2; exit 7;;
  bad) printf 'tampered' > "$out";;
  *) cat "$FAKE_ASSETS/${url##*/}" > "$out";;
esac
`,
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CURL", curl)
	t.Setenv("FAKE_CURL_LOG", m.curlLog)
	t.Setenv("FAKE_ASSETS", served)
	return m
}

func (m *fakeMachine) deps() WorkerDeps {
	return WorkerDeps{
		Client: &fakeClient{code: secret.New("C0DE")},
		Checksum: func(_ context.Context, _, asset string) (string, error) {
			h := sha256.Sum256(m.assets[asset])
			return hex.EncodeToString(h[:]), nil
		},
		Fetch: func(_ context.Context, _, asset string, _ func(string, ...any)) (string, error) {
			m.mu.Lock()
			m.fetched = append(m.fetched, asset)
			m.mu.Unlock()
			return filepath.Join(m.cache, asset), nil
		},
	}
}

func (m *fakeMachine) bin(name string) string {
	return filepath.Join(m.facts.Home, ".local", "bin", name)
}

func (m *fakeMachine) curlCalls() int {
	b, _ := os.ReadFile(m.curlLog)
	return strings.Count(string(b), "\n")
}

// plan builds a worker add plan reduced to its binaries step.
func (m *fakeMachine) plan(host, fetch string) *plan.Plan {
	m.t.Helper()
	hub := &config.Hub{ID: "hub-1", Address: "hub.example.com:7443", CAFingerprint: "sha256:aa", Mode: config.ModePublic}
	p, err := BuildWorkerAdd(WorkerRequest{Host: host, Name: "worker-1", Hub: hub, Version: "v0.1.42", Fetch: fetch}, m.facts, m.deps())
	if err != nil {
		m.t.Fatal(err)
	}
	p.Steps = p.Steps[:1]
	return p
}

type lines struct {
	mu  sync.Mutex
	all []string
}

func (l *lines) Started(int, *plan.Step)   {}
func (l *lines) Finished(int, plan.Result) {}
func (l *lines) Line(_ int, s string)      { l.mu.Lock(); l.all = append(l.all, s); l.mu.Unlock() }
func (l *lines) text() string              { l.mu.Lock(); defer l.mu.Unlock(); return strings.Join(l.all, "\n") }

func (m *fakeMachine) apply(p *plan.Plan) ([]plan.Result, string, error) {
	out := &lines{}
	r := &plan.Runner{Observer: out}
	res, err := r.Run(context.Background(), p, plan.NewEnv(), p.Digest())
	return res, out.text(), err
}

func (m *fakeMachine) assertInstalled(names ...string) {
	m.t.Helper()
	for _, n := range names {
		path := m.bin(strings.TrimSuffix(n, "-linux-amd64"))
		got, err := os.ReadFile(path)
		if err != nil || string(got) != string(m.assets[n]) {
			m.t.Fatalf("%s: %q %v", path, got, err)
		}
		if st, _ := os.Stat(path); st.Mode().Perm() != 0o755 {
			m.t.Fatalf("%s mode %v", path, st.Mode())
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(m.bin("x")))
	for _, e := range entries {
		if strings.Contains(e.Name(), ".lazylongarm.") {
			m.t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

func review(p *plan.Plan) string {
	var b strings.Builder
	p.Render(&b)
	return b.String()
}

func TestBinariesPlanPerFetchMode(t *testing.T) {
	m := newFakeMachine(t, "ok")
	url := "https://github.com/lubobill1990/longarm/releases/download/v0.1.42/longarmd-linux-amd64"
	cases := []struct {
		host, fetch string
		has, lacks  []string
	}{
		{"worker-1", FetchPush, []string{"[local] download longarmd-linux-amd64 and longarm-linux-amd64 (v0.1.42) from GitHub", "[worker-1] copy to", "via ssh stdin"}, []string{"curl"}},
		{"worker-1", FetchRemote, []string{"[local] read the sha256", "[worker-1] curl -fsSL --proto =https --proto-redir =https --connect-timeout 30 --speed-limit 1024 --speed-time 60", url, "install nothing"}, []string{"only if a download there fails", "via ssh stdin"}},
		{"worker-1", FetchAuto, []string{"[worker-1] curl -fsSL", "--connect-timeout 10 --speed-limit 32768 --speed-time 20", "[local] only if a download there fails", "[worker-1] only if a download there fails: copy to"}, nil},
		{"", FetchRemote, []string{"from this machine's verified release cache", "[local] copy to"}, []string{"curl"}},
	}
	digests := map[string]bool{}
	for _, tc := range cases {
		p := m.plan(tc.host, tc.fetch)
		text := review(p)
		for _, s := range tc.has {
			if !strings.Contains(text, s) {
				t.Errorf("%s/%s: review lacks %q\n%s", tc.host, tc.fetch, s, text)
			}
		}
		for _, s := range tc.lacks {
			if strings.Contains(text, s) {
				t.Errorf("%s/%s: review has %q\n%s", tc.host, tc.fetch, s, text)
			}
		}
		digests[p.Digest()] = true
		// --dry-run --json carries the same actions.
		b, _ := json.Marshal(p.JSON())
		if !strings.Contains(string(b), `"id":"binaries"`) || (tc.host != "" && tc.fetch != FetchPush && !strings.Contains(string(b), `"where":"worker-1","display":"curl -fsSL`)) {
			t.Errorf("%s/%s: json %s", tc.host, tc.fetch, b)
		}
	}
	if len(digests) != len(cases) {
		t.Fatal("fetch modes must change the plan digest")
	}
	if len(m.fetched) != 0 {
		t.Fatalf("building plans downloaded %v", m.fetched)
	}
	hub := &config.Hub{ID: "h", Address: "a:7443", CAFingerprint: "x"}
	if err := ValidateWorker(WorkerRequest{Name: "ok", Hub: hub, Service: ServiceSystem, Fetch: "scp"}, m.facts); err == nil || !strings.Contains(err.Error(), "auto, push or remote") {
		t.Fatalf("bad fetch mode: %v", err)
	}
	if got := WorkerDefaults(WorkerRequest{Host: "worker-1"}, m.facts).Fetch; got != FetchAuto {
		t.Fatalf("default fetch %q", got)
	}
}

func TestBinariesRemoteDownloadsOnTheMachine(t *testing.T) {
	m := newFakeMachine(t, "ok")
	if _, out, err := m.apply(m.plan("worker-1", FetchRemote)); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	m.assertInstalled("longarmd-linux-amd64", "longarm-linux-amd64")
	if len(m.fetched) != 0 || m.curlCalls() != 2 {
		t.Fatalf("controller downloaded %v, curl ran %d times", m.fetched, m.curlCalls())
	}
}

func TestBinariesAutoFallsBackToPush(t *testing.T) {
	m := newFakeMachine(t, "fail")
	_, out, err := m.apply(m.plan("worker-1", FetchAuto))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	m.assertInstalled("longarmd-linux-amd64", "longarm-linux-amd64")
	for _, want := range []string{"could not download longarmd-linux-amd64", "Failed to connect", "falling back to push", "copying longarm-linux-amd64"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// One failed download is enough: the second binary is pushed directly.
	if m.curlCalls() != 1 || len(m.fetched) != 2 {
		t.Fatalf("curl ran %d times, controller fetched %v", m.curlCalls(), m.fetched)
	}
}

func TestBinariesChecksumMismatch(t *testing.T) {
	// remote: a hard failure that installs nothing.
	m := newFakeMachine(t, "bad")
	res, out, err := m.apply(m.plan("worker-1", FetchRemote))
	if err == nil || res[0].Status != plan.Failed || !strings.Contains(res[0].Message, "failed verification") || !strings.Contains(res[0].Fix, "--fetch push") {
		t.Fatalf("mismatch: %v %+v\n%s", err, res, out)
	}
	for _, n := range []string{"longarmd", "longarm"} {
		if _, err := os.Stat(m.bin(n)); !os.IsNotExist(err) {
			t.Fatalf("%s was installed from a mismatching download", n)
		}
	}
	if len(m.fetched) != 0 {
		t.Fatalf("remote mode pushed: %v", m.fetched)
	}

	// auto: the tampered download is discarded, said loudly, and the
	// controller's verified copy is pushed.
	m = newFakeMachine(t, "bad")
	_, out, err = m.apply(m.plan("worker-1", FetchAuto))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "different bytes for longarmd-linux-amd64 than SHA256SUMS") || !strings.Contains(out, "falling back to push") {
		t.Fatalf("output:\n%s", out)
	}
	m.assertInstalled("longarmd-linux-amd64", "longarm-linux-amd64")
}

func TestBinariesHalfFinishedPairCopiesOnlyTheMissingOne(t *testing.T) {
	m := newFakeMachine(t, "fail")
	// A previous run installed longarmd, then timed out on longarm.
	if err := os.MkdirAll(filepath.Dir(m.bin("longarmd")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.bin("longarmd"), m.assets["longarmd-linux-amd64"], 0o755); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(m.bin("longarmd"))
	if err := os.WriteFile(m.bin("longarm"), []byte("half a bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, out, err := m.apply(m.plan("worker-1", FetchPush))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	after, _ := os.Stat(m.bin("longarmd"))
	if !os.SameFile(before, after) || !strings.Contains(out, "longarmd is already in place") {
		t.Fatalf("longarmd was copied again:\n%s", out)
	}
	if strings.Join(m.fetched, ",") != "longarm-linux-amd64" {
		t.Fatalf("pushed %v", m.fetched)
	}
	m.assertInstalled("longarmd-linux-amd64", "longarm-linux-amd64")

	// Rerun: the step is skipped without touching the cache.
	res, _, err := m.apply(m.plan("worker-1", FetchPush))
	if err != nil || res[0].Status != plan.Skipped || len(m.fetched) != 1 {
		t.Fatalf("rerun: %v %+v fetched %v", err, res, m.fetched)
	}
}
