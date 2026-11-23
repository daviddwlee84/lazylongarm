package flows

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/colab"
	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/longarm"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/secret"
)

type fakeColab struct {
	sessions []colab.Session
	calls    []string
	execs    []string
	uploads  map[string]string // remote path -> content at upload time
	failJoin bool
	failDown bool // the VM cannot reach GitHub
}

func (f *fakeColab) Usage(context.Context) (colab.Usage, error) {
	return colab.Usage{Balance: 10}, nil
}
func (f *fakeColab) Sessions(context.Context) ([]colab.Session, error) { return f.sessions, nil }
func (f *fakeColab) New(_ context.Context, name, gpu string, highMem bool) error {
	f.calls = append(f.calls, "new "+name+" "+gpu)
	f.sessions = append(f.sessions, colab.Session{Name: name, Hardware: "CPU"})
	return nil
}
func (f *fakeColab) Stop(_ context.Context, name string) error {
	f.calls = append(f.calls, "stop "+name)
	return nil
}
func (f *fakeColab) Upload(_ context.Context, name, local, remote string) error {
	b, err := os.ReadFile(local)
	if err != nil {
		return err
	}
	if f.uploads == nil {
		f.uploads = map[string]string{}
	}
	f.uploads[remote] = string(b)
	f.calls = append(f.calls, "upload "+remote)
	return nil
}
func (f *fakeColab) Exec(_ context.Context, name, src string, _ time.Duration) (string, error) {
	f.execs = append(f.execs, src)
	ok := func(s string) (string, error) { return "noise\n" + colab.Marker + " " + s + "\n", nil }
	switch {
	case strings.Contains(src, "platform.machine()"):
		return ok("x86_64 -")
	case strings.Contains(src, "urlopen"):
		f.calls = append(f.calls, "download")
		if f.failDown {
			return "urllib.error.URLError: <urlopen error timed out>\n", nil
		}
		return ok("downloaded 6")
	case strings.Contains(src, "os.replace(part, b)"):
		return ok("installed")
	case strings.Contains(src, `"enrolled" if ok`):
		return ok("no")
	case strings.Contains(src, `"join"`):
		if f.failJoin {
			return "Traceback: longarmd join failed (exit 1)\n", nil // colab exec exits 0 anyway
		}
		return ok("joined")
	case strings.Contains(src, "/proc"):
		return ok("none")
	case strings.Contains(src, "Popen"):
		return ok("4242")
	}
	return "unexpected script", nil
}

func colabFixture(t *testing.T) (ColabWorkerRequest, ColabDeps, *fakeClient, *fakeColab) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "longarmd-linux-amd64")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	hub := &config.Hub{ID: "hub-1", Address: "hub.example.com:7443", CAFingerprint: "sha256:aa", Mode: config.ModePublic}
	client := &fakeClient{code: secret.New("C0DE-1"), nodes: []longarm.Node{{Name: "colab-1", Role: "worker", State: "approved", Online: true, Labels: map[string]string{"provider": "colab"}, Endpoints: []string{"longnet://hub.example.com:7443?name=colab-1"}}}}
	cc := &fakeColab{}
	deps := ColabDeps{
		Client: client, Colab: cc, PollInterval: time.Millisecond, WaitTimeout: time.Second,
		Fetch:    func(context.Context, string, string, func(string, ...any)) (string, error) { return bin, nil },
		Checksum: func(context.Context, string, string) (string, error) { return strings.Repeat("ab", 32), nil },
	}
	req := ColabWorkerRequest{Name: "colab-1", GPU: "T4", Hub: hub, Version: "v0.1.42", Usage: colab.Usage{Balance: 10}, Labels: []string{"pool=ml", "provider=evil"}}
	return req, deps, client, cc
}

func TestColabWorkerAddRunsEndToEnd(t *testing.T) {
	req, deps, client, cc := colabFixture(t)
	p, err := BuildColabWorkerAdd(req, deps)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(p); got != "session,binary,enroll,daemon,online,try-it" {
		t.Fatalf("steps %s", got)
	}
	var b strings.Builder
	p.Render(&b)
	if strings.Contains(b.String(), "C0DE-1") || !strings.Contains(b.String(), "provider=colab, gpu=t4, pool=ml") || strings.Contains(b.String(), "provider=evil") {
		t.Fatalf("review:\n%s", b.String())
	}
	env := plan.NewEnv()
	results, err := (&plan.Runner{}).Run(context.Background(), p, env, p.Digest())
	if err != nil {
		t.Fatalf("%v\n%v", err, results)
	}
	if got := strings.Join(cc.calls, "; "); got != "new colab-1 T4; download; upload /content/longarm/.join-code" {
		t.Fatalf("colab calls: %s", got)
	}
	if cc.uploads["/content/longarm/.join-code"] != "C0DE-1" {
		t.Fatalf("code file: %q", cc.uploads["/content/longarm/.join-code"])
	}
	for _, src := range cc.execs {
		if strings.Contains(src, "C0DE-1") {
			t.Fatalf("the code reached an exec'd cell:\n%s", src)
		}
	}
	if env.Get("worker_path") != "relay" || len(client.execed) != 1 {
		t.Fatalf("path %q exec %v", env.Get("worker_path"), client.execed)
	}
}

func TestColabJoinWithoutMarkerFails(t *testing.T) {
	req, deps, _, cc := colabFixture(t)
	cc.failJoin = true
	p, err := BuildColabWorkerAdd(req, deps)
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&plan.Runner{}).Run(context.Background(), p, plan.NewEnv(), p.Digest())
	if err == nil || !strings.Contains(err.Error(), "did not report success") {
		t.Fatalf("a failed join (exit 0 from colab exec) passed: %v", err)
	}
}

func TestColabWorkerNeedsPositiveBalance(t *testing.T) {
	req, deps, _, _ := colabFixture(t)
	req.Usage.Balance = 0
	if _, err := BuildColabWorkerAdd(req, deps); err == nil || !strings.Contains(err.Error(), "positive balance") {
		t.Fatalf("zero balance accepted: %v", err)
	}
}

func TestColabRemove(t *testing.T) {
	_, deps, client, cc := colabFixture(t)
	node := client.nodes[0]
	sess := colab.Session{Name: "colab-1", ID: "m-1", Hardware: "T4"}
	cc.sessions = []colab.Session{sess}

	p, err := BuildColabWorkerRemove(ColabRemoveRequest{Name: "colab-1", Node: &node, Session: &sess}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(p); got != "revoke,stop" {
		t.Fatalf("steps %s", got)
	}
	for _, s := range p.Steps {
		if err := s.Apply(context.Background(), plan.NewEnv()); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(strings.Join(client.calls, ";"), "worker remove colab-1") || cc.calls[len(cc.calls)-1] != "stop colab-1" {
		t.Fatalf("calls %v %v", client.calls, cc.calls)
	}

	// The VM is gone already: only the revoke remains.
	p, err = BuildColabWorkerRemove(ColabRemoveRequest{Name: "colab-1", Node: &node}, deps)
	if err != nil || ids(p) != "revoke" {
		t.Fatalf("%v %s", err, ids(p))
	}

	// Not a Colab worker: refuse.
	other := longarm.Node{Name: "build-box", Role: "worker", State: "approved"}
	if _, err := BuildColabWorkerRemove(ColabRemoveRequest{Name: "build-box", Node: &other}, deps); err == nil || !strings.Contains(err.Error(), "provider=colab") {
		t.Fatalf("non-Colab worker accepted: %v", err)
	}
	if _, err := BuildColabWorkerRemove(ColabRemoveRequest{Name: "nothing"}, deps); err == nil {
		t.Fatal("nothing to remove accepted")
	}
}

func TestDefaultColabName(t *testing.T) {
	if got := DefaultColabName(time.Date(2026, 11, 23, 9, 5, 0, 0, time.Local)); got != "colab-1123-0905" || !workerNameRE.MatchString(got) {
		t.Fatal(got)
	}
}

func TestColabBinaryFetchModes(t *testing.T) {
	cases := []struct {
		fetch    string
		failDown bool
		want     string
		fail     bool
	}{
		{FetchAuto, false, "download", false},
		{FetchAuto, true, "download; upload /content/longarm/bin/longarmd.part", false},
		{FetchRemote, true, "download", true},
		{FetchPush, false, "upload /content/longarm/bin/longarmd.part", false},
	}
	for _, c := range cases {
		req, deps, _, cc := colabFixture(t)
		req.Fetch = c.fetch
		cc.failDown = c.failDown
		cc.sessions = []colab.Session{{Name: req.Name}}
		p, err := BuildColabWorkerAdd(req, deps)
		if err != nil {
			t.Fatal(err)
		}
		var step plan.Step
		for _, s := range p.Steps {
			if s.ID == "binary" {
				step = s
			}
		}
		err = step.Apply(context.Background(), plan.NewEnv())
		if got := strings.Join(cc.calls, "; "); got != c.want || (err != nil) != c.fail {
			t.Errorf("%s failDown=%v: calls %q err %v", c.fetch, c.failDown, got, err)
		}
	}
	req, deps, _, _ := colabFixture(t)
	req.Fetch = "bogus"
	if _, err := BuildColabWorkerAdd(req, deps); err == nil {
		t.Error("bogus fetch mode accepted")
	}
}
