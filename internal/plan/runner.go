package plan

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/connection"
)

// Observer receives progress for display. Calls are sequential.
type Observer interface {
	Started(i int, s *Step)
	Line(i int, line string)
	Finished(i int, r Result)
}

// ErrDigestMismatch: the plan changed after review.
var ErrDigestMismatch = errors.New("the plan changed after it was reviewed; review it again")

// ErrStopped: the run stopped on request between steps.
var ErrStopped = errors.New("stopped on request after the current step")

// Runner applies a plan.
type Runner struct {
	Observer Observer
	Journal  *Journal
	// Stop, when closed, ends the run after the current step finishes.
	Stop <-chan struct{}
}

// StepError is returned when a step did not complete.
type StepError struct {
	Index  int
	Result Result
	Err    error
}

func (e *StepError) Error() string {
	return fmt.Sprintf("step %d (%s) %s: %v", e.Index+1, e.Result.Title, e.Result.Status, e.Err)
}
func (e *StepError) Unwrap() error { return e.Err }

// Run applies p after checking the reviewed digest. It returns every step's
// result; on failure the error is a *StepError and later steps are Stopped.
func (r *Runner) Run(ctx context.Context, p *Plan, env *Env, reviewed string) ([]Result, error) {
	if reviewed != p.Digest() {
		return nil, ErrDigestMismatch
	}
	results := make([]Result, len(p.Steps))
	for i, s := range p.Steps {
		results[i] = Result{ID: s.ID, Title: s.Title, Status: Pending}
	}
	if r.Journal != nil {
		if blocked := r.Journal.BlockingUnknown(p); blocked != "" {
			return results, fmt.Errorf("a previous run left step %q with an unknown outcome; inspect the host, then run again with --force-resume", blocked)
		}
		r.Journal.Begin(p)
	}
	current := -1
	env.logf = func(line string) {
		if r.Observer != nil && current >= 0 {
			r.Observer.Line(current, line)
		}
	}
	defer func() { env.logf = nil }()
	for i := range p.Steps {
		if i > 0 && r.stopRequested() {
			for j := i; j < len(results); j++ {
				results[j].Status = Stopped
			}
			r.save(results)
			return results, ErrStopped
		}
		current = i
		s := &p.Steps[i]
		res := &results[i]
		res.Status, res.Started = Running, time.Now()
		if r.Observer != nil {
			r.Observer.Started(i, s)
		}
		r.save(results)
		err := r.runStep(ctx, s, env, res)
		res.Finished = time.Now()
		if err != nil {
			res.Status, res.Message, res.Fix = classify(err, s, ctx)
			if r.Observer != nil {
				r.Observer.Finished(i, *res)
			}
			for j := i + 1; j < len(results); j++ {
				results[j].Status = Stopped
			}
			r.save(results)
			return results, &StepError{Index: i, Result: *res, Err: err}
		}
		if r.Observer != nil {
			r.Observer.Finished(i, *res)
		}
		r.save(results)
	}
	return results, nil
}

func (r *Runner) stopRequested() bool {
	if r.Stop == nil {
		return false
	}
	select {
	case <-r.Stop:
		return true
	default:
		return false
	}
}

func (r *Runner) save(results []Result) {
	if r.Journal != nil {
		r.Journal.Update(results)
	}
}

func (r *Runner) runStep(ctx context.Context, s *Step, env *Env, res *Result) error {
	attempt := func() error {
		if s.Check != nil {
			ok, err := s.Check(ctx, env)
			if err != nil {
				return err
			}
			if ok {
				res.Status, res.Message = Skipped, "already in place"
				if s.Verify != nil {
					return s.Verify(ctx, env)
				}
				return nil
			}
		}
		if s.Apply != nil {
			if err := s.Apply(ctx, env); err != nil {
				return err
			}
		}
		if s.Verify != nil {
			if err := s.Verify(ctx, env); err != nil {
				return err
			}
		}
		res.Status = Done
		if s.ReadOnly {
			res.Status = Verified
		}
		return nil
	}
	err := attempt()
	var auth *connection.AuthRequiredError
	if errors.As(err, &auth) && env.Authenticate != nil {
		env.Logf("  SSH needs you to log in to %s; handing the terminal over", auth.Host)
		if aerr := env.Authenticate(ctx, auth.Host); aerr != nil {
			return err
		}
		err = attempt() // one bounded retry after a successful handoff
	}
	return err
}

func classify(err error, s *Step, ctx context.Context) (Status, string, string) {
	var fix *FixError
	hint := ""
	if errors.As(err, &fix) {
		hint = fix.Fix
	}
	var auth *connection.AuthRequiredError
	var sudo *NeedsSudoError
	var unknown *UnknownError
	switch {
	case errors.As(err, &auth):
		return NeedsAuth, err.Error(), "ssh " + auth.Host
	case errors.As(err, &sudo):
		return NeedsSudo, err.Error(), hint
	case errors.As(err, &unknown):
		return Unknown, err.Error(), hint
	case ctx.Err() != nil && !s.Idempotent:
		return Unknown, "interrupted while applying; " + err.Error(), hint
	}
	return Failed, err.Error(), hint
}
