// Package plan is the shared preview/apply engine. Flows build a Plan of
// Steps; the Review screen, `--dry-run` and `--dry-run --json` render the same
// Actions; the Runner applies steps in order (Check → Apply → Verify) with a
// journal, so a rerun skips satisfied steps and resumes after a failure.
package plan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/secret"
)

// Status of one step.
type Status string

const (
	Pending   Status = "pending"
	Running   Status = "running"
	Skipped   Status = "skipped" // Check found it already satisfied
	Done      Status = "done"
	Failed    Status = "failed"
	Unknown   Status = "unknown" // outcome not confirmed; inspect before retrying
	NeedsAuth Status = "needs_auth"
	NeedsSudo Status = "needs_sudo"
	Stopped   Status = "stopped"  // not attempted because the run stopped
	Verified  Status = "verified" // a read-only step ran and confirmed its expectation
)

// Action is one effect shown in Review: where it runs and what it does.
type Action struct {
	Where      string `json:"where"` // "local", an SSH alias, or a provider name ("azure")
	Privileged bool   `json:"privileged,omitempty"`
	Display    string `json:"display"`
}

// Step is one idempotent-where-possible unit of work.
type Step struct {
	ID         string
	Title      string
	Actions    []Action
	Idempotent bool // safe to re-run after an unknown outcome
	ReadOnly   bool // only observes (and records); reported as verified, not applied
	// Check reports whether the step's effect is already present (skip).
	Check func(context.Context, *Env) (bool, error)
	// Apply performs the effect.
	Apply func(context.Context, *Env) error
	// Verify confirms the effect after Apply (or after a skipping Check).
	Verify func(context.Context, *Env) error
}

// Plan is a reviewed sequence of steps.
type Plan struct {
	Kind     string      // e.g. "hub-setup"
	Key      string      // stable identity for the journal, e.g. the SSH host
	Title    string      // one line
	Summary  [][2]string // resolved values shown in Review
	Warnings []string
	Steps    []Step
}

// Digest identifies what was reviewed. Apply refuses a plan whose digest
// differs from the reviewed one.
func (p *Plan) Digest() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00", p.Kind, p.Key)
	for _, kv := range p.Summary {
		fmt.Fprintf(h, "%s=%s\x00", kv[0], kv[1])
	}
	for _, s := range p.Steps {
		fmt.Fprintf(h, "step %s %s\x00", s.ID, s.Title)
		for _, a := range s.Actions {
			fmt.Fprintf(h, "%s %v %s\x00", a.Where, a.Privileged, a.Display)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Render writes the human Review text.
func (p *Plan) Render(w io.Writer) {
	fmt.Fprintf(w, "%s\n\n", p.Title)
	if len(p.Summary) > 0 {
		width := 0
		for _, kv := range p.Summary {
			if len(kv[0]) > width {
				width = len(kv[0])
			}
		}
		for _, kv := range p.Summary {
			fmt.Fprintf(w, "  %-*s  %s\n", width, kv[0], kv[1])
		}
		fmt.Fprintln(w)
	}
	for i, s := range p.Steps {
		fmt.Fprintf(w, "%2d. %s\n", i+1, s.Title)
		for _, a := range s.Actions {
			marker := " "
			if a.Privileged {
				marker = "#"
			}
			for j, line := range strings.Split(a.Display, "\n") {
				if j == 0 {
					fmt.Fprintf(w, "      %s [%s] %s\n", marker, a.Where, line)
				} else {
					fmt.Fprintf(w, "        %s%s\n", strings.Repeat(" ", len(a.Where)+3), line)
				}
			}
		}
	}
	if len(p.Warnings) > 0 {
		fmt.Fprintln(w)
		for _, warn := range p.Warnings {
			fmt.Fprintf(w, "  ! %s\n", warn)
		}
	}
	privileged := false
	for _, s := range p.Steps {
		for _, a := range s.Actions {
			privileged = privileged || a.Privileged
		}
	}
	if privileged {
		fmt.Fprintf(w, "\n  # = runs as root (sudo).  plan %s\n", p.Digest())
	} else {
		fmt.Fprintf(w, "\n  plan %s\n", p.Digest())
	}
}

// JSON is the machine-readable dry-run form.
func (p *Plan) JSON() any {
	type step struct {
		ID         string   `json:"id"`
		Title      string   `json:"title"`
		Idempotent bool     `json:"idempotent"`
		Actions    []Action `json:"actions"`
	}
	summary := map[string]string{}
	for _, kv := range p.Summary {
		summary[kv[0]] = kv[1]
	}
	out := struct {
		Kind     string            `json:"kind"`
		Key      string            `json:"key"`
		Title    string            `json:"title"`
		Digest   string            `json:"digest"`
		Summary  map[string]string `json:"summary"`
		Warnings []string          `json:"warnings"`
		Steps    []step            `json:"steps"`
	}{p.Kind, p.Key, p.Title, p.Digest(), summary, p.Warnings, nil}
	if out.Warnings == nil {
		out.Warnings = []string{}
	}
	for _, s := range p.Steps {
		out.Steps = append(out.Steps, step{s.ID, s.Title, s.Idempotent, s.Actions})
	}
	return out
}

// Result of one step.
type Result struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Status   Status    `json:"status"`
	Message  string    `json:"message,omitempty"`
	Fix      string    `json:"fix,omitempty"`
	Started  time.Time `json:"started,omitempty"`
	Finished time.Time `json:"finished,omitempty"`
}

// Env carries values between steps and the terminal capabilities of the
// current entry point.
type Env struct {
	mu      sync.Mutex
	values  map[string]string
	secrets map[string]secret.Secret
	logf    func(string)

	// Interactive runs req attached to the real terminal (sudo/ssh prompt
	// natively). Nil in non-TTY/JSON mode: then prompts become typed errors.
	Interactive func(context.Context, connection.Request) error
	// Authenticate hands the terminal to ssh for host; nil when not a TTY.
	Authenticate func(context.Context, string) error
}

// NewEnv returns an empty environment.
func NewEnv() *Env {
	return &Env{values: map[string]string{}, secrets: map[string]secret.Secret{}}
}

// Set stores a non-secret value for later steps.
func (e *Env) Set(k, v string) { e.mu.Lock(); e.values[k] = v; e.mu.Unlock() }

// Get returns a value set by an earlier step (or the flow).
func (e *Env) Get(k string) string { e.mu.Lock(); defer e.mu.Unlock(); return e.values[k] }

// Values returns a copy of all non-secret values.
func (e *Env) Values() map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]string, len(e.values))
	for k, v := range e.values {
		out[k] = v
	}
	return out
}

// SetSecret stores a secret minted during apply (never journaled).
func (e *Env) SetSecret(k string, s secret.Secret) { e.mu.Lock(); e.secrets[k] = s; e.mu.Unlock() }

// Secret returns a stored secret.
func (e *Env) Secret(k string) secret.Secret { e.mu.Lock(); defer e.mu.Unlock(); return e.secrets[k] }

func (e *Env) allSecrets() []secret.Secret {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]secret.Secret, 0, len(e.secrets))
	for _, s := range e.secrets {
		out = append(out, s)
	}
	return out
}

// Logf streams one redacted progress line.
func (e *Env) Logf(format string, args ...any) {
	if e.logf == nil {
		return
	}
	line := secret.Redact(fmt.Sprintf(format, args...), e.allSecrets()...)
	e.logf(connection.Sanitize(line))
}

// Run executes req, streaming its output lines (redacted) to the log.
func (e *Env) Run(ctx context.Context, req connection.Request) (connection.Result, error) {
	var pending []byte
	var mu sync.Mutex
	req.Output = func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		pending = append(pending, b...)
		for {
			i := strings.IndexByte(string(pending), '\n')
			if i < 0 {
				break
			}
			line := strings.TrimRight(string(pending[:i]), "\r")
			pending = pending[i+1:]
			if strings.TrimSpace(line) != "" {
				e.Logf("  │ %s", line)
			}
		}
	}
	res, err := connection.Run(ctx, req)
	mu.Lock()
	if rest := strings.TrimSpace(string(pending)); rest != "" {
		e.Logf("  │ %s", rest)
	}
	mu.Unlock()
	return res, e.redact(err)
}

// redactedError hides secrets in the message but keeps the cause reachable
// for errors.As/errors.Is (CommandError, ErrSudoPasswordRequired, ...).
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

func (e *Env) redact(err error) error {
	if err == nil {
		return nil
	}
	msg := secret.Redact(err.Error(), e.allSecrets()...)
	if msg == err.Error() {
		return err
	}
	return &redactedError{msg: msg, err: err}
}

// RunPrivileged runs req as root: `sudo -n` first; when sudo needs a password
// and a terminal is available, the same script runs interactively instead
// (sudo timestamps are per-TTY, so a prior `sudo -v` would not help).
// Output of an interactive run goes to the terminal, not to Result.
func (e *Env) RunPrivileged(ctx context.Context, req connection.Request) (connection.Result, error) {
	req.Sudo = true
	res, err := e.Run(ctx, req)
	if !errors.Is(err, connection.ErrSudoPasswordRequired) {
		return res, err
	}
	if e.Interactive == nil || req.Stdin != nil {
		return res, &NeedsSudoError{Host: req.Host}
	}
	e.Logf("  sudo needs a password on %s; handing the terminal over", where(req.Host))
	if err := e.Interactive(ctx, req); err != nil {
		return connection.Result{}, err
	}
	return connection.Result{}, nil
}

// RunQuiet runs req without streaming its output (for reads that return a
// secret). With Sudo it never falls back to an interactive prompt, because an
// interactive run would print the secret on the terminal.
func (e *Env) RunQuiet(ctx context.Context, req connection.Request) (connection.Result, error) {
	req.Output = nil
	res, err := connection.Run(ctx, req)
	if errors.Is(err, connection.ErrSudoPasswordRequired) {
		return res, &NeedsSudoError{Host: req.Host}
	}
	return res, e.redact(err)
}

// NeedsSudoError: the step needs an interactive sudo prompt but no terminal.
type NeedsSudoError struct{ Host string }

func (e *NeedsSudoError) Error() string {
	return fmt.Sprintf("sudo on %s needs a password; rerun in a terminal, or allow passwordless sudo for this user", where(e.Host))
}

func where(host string) string {
	if host == "" {
		return "this machine"
	}
	return host
}

// UnknownError marks an outcome that may or may not have taken effect.
type UnknownError struct{ Err error }

func (e *UnknownError) Error() string { return "outcome unknown: " + e.Err.Error() }
func (e *UnknownError) Unwrap() error { return e.Err }

// FixError attaches a suggested fix command to an error.
type FixError struct {
	Err error
	Fix string
}

func (e *FixError) Error() string { return e.Err.Error() }
func (e *FixError) Unwrap() error { return e.Err }

// WithFix wraps err with a fix hint.
func WithFix(err error, fix string) error {
	if err == nil {
		return nil
	}
	return &FixError{Err: err, Fix: fix}
}

// MarshalJSON for Result times omits zero values cleanly.
func (r Result) String() string {
	b, _ := json.Marshal(r)
	return string(b)
}
