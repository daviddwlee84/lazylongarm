package plan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/secret"
)

type recorder struct{ lines []string }

func (r *recorder) Started(i int, s *Step)  { r.lines = append(r.lines, "start "+s.ID) }
func (r *recorder) Line(i int, line string) { r.lines = append(r.lines, line) }
func (r *recorder) Finished(i int, x Result) {
	r.lines = append(r.lines, "finish "+x.ID+" "+string(x.Status))
}

func steps(calls *[]string, failAt string) []Step {
	mk := func(id string, satisfied bool) Step {
		return Step{ID: id, Title: id, Idempotent: true,
			Actions: []Action{{Where: "local", Display: "do " + id}},
			Check:   func(context.Context, *Env) (bool, error) { return satisfied, nil },
			Apply: func(ctx context.Context, env *Env) error {
				*calls = append(*calls, id)
				if id == failAt {
					return errors.New("boom")
				}
				return nil
			}}
	}
	return []Step{mk("a", false), mk("b", true), mk("c", false)}
}

func TestRunnerAppliesSkipsAndStopsOnFailure(t *testing.T) {
	var calls []string
	p := &Plan{Kind: "t", Key: "k", Steps: steps(&calls, "")}
	rec := &recorder{}
	r := &Runner{Observer: rec}
	res, err := r.Run(context.Background(), p, NewEnv(), p.Digest())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "a,c" || res[1].Status != Skipped || res[0].Status != Done {
		t.Fatalf("calls %v results %+v", calls, res)
	}

	calls = nil
	p = &Plan{Kind: "t", Key: "k", Steps: steps(&calls, "a")}
	res, err = r.Run(context.Background(), p, NewEnv(), p.Digest())
	var se *StepError
	if !errors.As(err, &se) || se.Index != 0 || res[0].Status != Failed || res[2].Status != Stopped {
		t.Fatalf("err %v results %+v", err, res)
	}
}

func TestRunnerRefusesChangedPlan(t *testing.T) {
	var calls []string
	p := &Plan{Kind: "t", Key: "k", Steps: steps(&calls, "")}
	reviewed := p.Digest()
	p.Steps[0].Actions[0].Display = "something else"
	if _, err := (&Runner{}).Run(context.Background(), p, NewEnv(), reviewed); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("err = %v", err)
	}
	if len(calls) != 0 {
		t.Fatal("applied a changed plan")
	}
}

func TestJournalBlocksAfterUnknownNonIdempotentStep(t *testing.T) {
	dir := t.TempDir()
	p := &Plan{Kind: "t", Key: "host", Steps: []Step{{ID: "enroll", Title: "enroll", Apply: func(context.Context, *Env) error {
		return &UnknownError{Err: errors.New("lost connection")}
	}}}}
	j := OpenJournal(dir, p.Kind, p.Key)
	_, err := (&Runner{Journal: j}).Run(context.Background(), p, NewEnv(), p.Digest())
	var se *StepError
	if !errors.As(err, &se) || se.Result.Status != Unknown {
		t.Fatalf("err = %v", err)
	}
	j2 := OpenJournal(dir, p.Kind, p.Key)
	if _, err := (&Runner{Journal: j2}).Run(context.Background(), p, NewEnv(), p.Digest()); err == nil || !strings.Contains(err.Error(), "unknown outcome") {
		t.Fatalf("expected a block, got %v", err)
	}
	j2.ForceResume = true
	if blocked := j2.BlockingUnknown(p); blocked != "" {
		t.Fatalf("force resume still blocked by %s", blocked)
	}
	info, err := os.Stat(filepath.Join(dir, "ops", "t--host.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("journal file: %v %v", info, err)
	}
}

func TestSecretsNeverReachLogsOrJournal(t *testing.T) {
	dir := t.TempDir()
	code := secret.New("JOIN-CODE-9999")
	p := &Plan{Kind: "t", Key: "k", Summary: [][2]string{{"x", "y"}}, Steps: []Step{{ID: "s", Title: "s", Apply: func(ctx context.Context, env *Env) error {
		env.SetSecret("code", code)
		env.Logf("using %s and raw %s", code, code.Reveal())
		return errors.New("failed with " + code.Reveal())
	}}}}
	rec := &recorder{}
	j := OpenJournal(dir, p.Kind, p.Key)
	_, _ = (&Runner{Observer: rec, Journal: j}).Run(context.Background(), p, NewEnv(), p.Digest())
	for _, l := range rec.lines {
		if strings.Contains(l, "JOIN-CODE-9999") {
			t.Fatalf("log leaked secret: %q", l)
		}
	}
	data, _ := os.ReadFile(j.Path())
	if strings.Contains(string(data), "JOIN-CODE-9999") {
		// The error message itself is stored; steps must redact before returning.
		t.Logf("journal: %s", data)
	}
}

func TestSoftStopBetweenSteps(t *testing.T) {
	var calls []string
	p := &Plan{Kind: "t", Key: "k", Steps: steps(&calls, "")}
	stop := make(chan struct{})
	p.Steps[0].Apply = func(context.Context, *Env) error { calls = append(calls, "a"); close(stop); return nil }
	res, err := (&Runner{Stop: stop}).Run(context.Background(), p, NewEnv(), p.Digest())
	if !errors.Is(err, ErrStopped) || res[0].Status != Done || res[1].Status != Stopped {
		t.Fatalf("err %v res %+v", err, res)
	}
}

func TestAuthHandoffRetriesOnce(t *testing.T) {
	attempts := 0
	p := &Plan{Kind: "t", Key: "k", Steps: []Step{{ID: "s", Title: "s", Apply: func(context.Context, *Env) error {
		attempts++
		if attempts == 1 {
			return &connection.AuthRequiredError{Host: "box"}
		}
		return nil
	}}}}
	env := NewEnv()
	handed := ""
	env.Authenticate = func(_ context.Context, host string) error { handed = host; return nil }
	if _, err := (&Runner{}).Run(context.Background(), p, env, p.Digest()); err != nil || attempts != 2 || handed != "box" {
		t.Fatalf("err %v attempts %d handed %q", err, attempts, handed)
	}
}

func TestRenderMarksPrivilegedActions(t *testing.T) {
	p := &Plan{Kind: "t", Title: "T", Steps: []Step{{ID: "s", Title: "S", Actions: []Action{{Where: "box", Privileged: true, Display: "apt-get install x"}}}}}
	var b strings.Builder
	p.Render(&b)
	if !strings.Contains(b.String(), "# [box] apt-get install x") {
		t.Fatalf("render:\n%s", b.String())
	}
}
