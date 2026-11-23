// Package colab drives Google's Colab CLI (`colab`, from the
// google-colab-cli Python package) to borrow a Colab VM: create a session,
// run Python on its kernel, upload files, stop it, and read the account's
// compute-unit balance.
//
// The CLI is young; this package depends only on its plain-text output
// for `usage` and `sessions` (parsed leniently) and on `exec` streaming the
// remote program's output. `colab exec` exits 0 even when the remote code
// raised, so callers must print a marker on success and check for it
// (see Marker).
package colab

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Marker starts the line a remote script prints when it succeeded; the
// rest of the line is its result.
const Marker = "__LAZYLONGARM_OK__"

// Usage is `colab usage`.
type Usage struct {
	Balance     float64 // compute units
	RatePerHour float64 // units per hour across active assignments
	Active      int     // active assignments (running VMs)
}

// Session is one line of `colab sessions`.
type Session struct {
	Name     string
	ID       string
	Hardware string // CPU, T4, L4, A100, ...
	Shape    string // Standard, High-RAM, ...
}

// Client runs the colab CLI.
type Client struct {
	Bin string // default: "colab" on PATH
}

func (c *Client) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	return "colab"
}

// Available reports whether the CLI is installed.
func (c *Client) Available() error {
	if _, err := exec.LookPath(c.bin()); err != nil {
		return errors.New("the Colab CLI is not installed: uv tool install google-colab-cli (then sign in once with any colab command)")
	}
	return nil
}

// Error is a failed colab command with its output.
type Error struct {
	Args   []string
	Output string
	Err    error
}

func (e *Error) Error() string {
	out := strings.TrimSpace(e.Output)
	if len(out) > 1200 {
		out = "…" + out[len(out)-1200:]
	}
	if out == "" {
		return fmt.Sprintf("colab %s: %v", strings.Join(e.Args, " "), e.Err)
	}
	return fmt.Sprintf("colab %s: %v: %s", strings.Join(e.Args, " "), e.Err, out)
}

func (e *Error) Unwrap() error { return e.Err }

func (c *Client) run(ctx context.Context, timeout time.Duration, stdin []byte, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	text := StripANSI(out.String())
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("timed out after %s", timeout)
		}
		return text, &Error{Args: args, Output: text, Err: err}
	}
	return text, nil
}

// Usage returns the account's compute-unit balance and burn rate.
func (c *Client) Usage(ctx context.Context) (Usage, error) {
	out, err := c.run(ctx, time.Minute, nil, "usage")
	if err != nil {
		return Usage{}, err
	}
	return ParseUsage(out)
}

// Sessions lists the account's active sessions.
func (c *Client) Sessions(ctx context.Context) ([]Session, error) {
	out, err := c.run(ctx, time.Minute, nil, "sessions")
	if err != nil {
		return nil, err
	}
	return ParseSessions(out), nil
}

// New creates a session (a VM). gpu "" is a CPU runtime.
func (c *Client) New(ctx context.Context, name, gpu string, highMem bool) error {
	args := []string{"new", "-s", name}
	if gpu != "" {
		args = append(args, "--gpu", gpu)
	}
	if highMem {
		args = append(args, "--high-mem")
	}
	_, err := c.run(ctx, 10*time.Minute, nil, args...)
	return err
}

// Exec runs Python source on the session's kernel and returns its output.
// silence is how long the remote side may print nothing before the CLI
// gives up. The exit status says nothing about the remote code: look for
// Marker in the output.
func (c *Client) Exec(ctx context.Context, name, source string, silence time.Duration) (string, error) {
	return c.run(ctx, silence+2*time.Minute, []byte(source), "exec", "-s", name, "--timeout", strconv.Itoa(int(silence.Seconds())))
}

// Upload copies a local file to the session (the remote directory must
// exist; uploads above ~60 MB are rejected by the service).
func (c *Client) Upload(ctx context.Context, name, local, remote string) error {
	_, err := c.run(ctx, 15*time.Minute, nil, "upload", "-s", name, local, remote)
	return err
}

// Stop ends the session and releases the VM.
func (c *Client) Stop(ctx context.Context, name string) error {
	_, err := c.run(ctx, 2*time.Minute, nil, "stop", "-s", name)
	return err
}

var (
	ansiRE    = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	balanceRE = regexp.MustCompile(`(?i)balance:\s*([0-9]+(?:\.[0-9]+)?)`)
	rateRE    = regexp.MustCompile(`(?i)usage rate:\s*([0-9]+(?:\.[0-9]+)?)`)
	activeRE  = regexp.MustCompile(`(?i)active assignments:\s*([0-9]+)`)
	sessionRE = regexp.MustCompile(`^\[([^\]]+)\]\s*(.*)$`)
)

// StripANSI removes terminal escape sequences.
func StripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// ParseUsage reads `colab usage`:
//
//	Current balance: 571.68 compute units
//	Usage rate: 0.08/hr
//	Active assignments: 1
func ParseUsage(s string) (Usage, error) {
	s = StripANSI(s)
	m := balanceRE.FindStringSubmatch(s)
	if m == nil {
		return Usage{}, fmt.Errorf("unexpected `colab usage` output: %q", strings.TrimSpace(s))
	}
	u := Usage{}
	u.Balance, _ = strconv.ParseFloat(m[1], 64)
	if m := rateRE.FindStringSubmatch(s); m != nil {
		u.RatePerHour, _ = strconv.ParseFloat(m[1], 64)
	}
	if m := activeRE.FindStringSubmatch(s); m != nil {
		u.Active, _ = strconv.Atoi(m[1])
	}
	return u, nil
}

// ParseSessions reads `colab sessions`, one session per line:
//
//	[name] m-s-xxxx | Hardware: CPU | Shape: Standard | Variant: DEFAULT
func ParseSessions(s string) []Session {
	var out []Session
	for line := range strings.SplitSeq(StripANSI(s), "\n") {
		m := sessionRE.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil || !strings.Contains(m[2], "|") {
			continue // not a session (e.g. the CLI's own "[colab] ..." chatter)
		}
		sess := Session{Name: m[1]}
		for i, part := range strings.Split(m[2], "|") {
			part = strings.TrimSpace(part)
			k, v, ok := strings.Cut(part, ":")
			switch {
			case !ok && i == 0:
				sess.ID = part
			case strings.EqualFold(k, "Hardware"):
				sess.Hardware = strings.TrimSpace(v)
			case strings.EqualFold(k, "Shape"):
				sess.Shape = strings.TrimSpace(v)
			}
		}
		out = append(out, sess)
	}
	return out
}

// FindSession returns the session called name.
func FindSession(list []Session, name string) (Session, bool) {
	for _, s := range list {
		if s.Name == name {
			return s, true
		}
	}
	return Session{}, false
}

// Result returns the text after Marker in a remote script's output, or
// false when the script did not report success.
func Result(output string) (string, bool) {
	for line := range strings.SplitSeq(output, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), Marker); ok {
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}
