package connection

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

// Request describes one fixed shell script executed with /bin/sh, either
// locally (Host == "") or on an SSH host. Data that may be sensitive belongs
// on Stdin, never in Script (Script is visible in remote process arguments).
type Request struct {
	Host    string
	Script  string
	Args    []string // positional parameters ($1..) for Script
	Stdin   []byte
	Timeout time.Duration
	Limit   int          // stdout limit in bytes (default 4 MiB)
	Output  func([]byte) // optional live tee of stdout+stderr chunks
	Sudo    bool         // run Script as root with `sudo -n` (batch, never prompts)
	// Stall, when > 0, stops the command after that long without any stdout
	// or stderr output: an idle timeout for scripts that report progress.
	// Timeout stays the overall ceiling.
	Stall time.Duration
}

// StallError is a command stopped by Request.Stall.
type StallError struct {
	Where string
	Stall time.Duration
}

func (e *StallError) Error() string {
	return fmt.Sprintf("%s: no progress for %s", e.Where, e.Stall)
}

// Result holds bounded output of a finished command.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// CommandError is a command that ran but exited non-zero.
type CommandError struct {
	ExitCode int
	Stderr   string // bounded tail, control characters removed
}

func (e *CommandError) Error() string {
	tail := strings.TrimSpace(e.Stderr)
	if tail == "" {
		return fmt.Sprintf("command exited with status %d", e.ExitCode)
	}
	return fmt.Sprintf("command exited with status %d: %s", e.ExitCode, tail)
}

// ErrSudoPasswordRequired means a privileged step needs an interactive sudo
// prompt. Callers hand the terminal to InteractiveCommand and retry nothing
// automatically: the interactive run *is* the step.
var ErrSudoPasswordRequired = errors.New("sudo requires a password on this host; run the step interactively")

// Run executes req and returns its output. Transport failures are typed
// (*AuthRequiredError, *SSHTransportError); a non-zero exit is *CommandError
// together with the partial Result.
func Run(ctx context.Context, req Request) (Result, error) {
	if req.Timeout <= 0 {
		req.Timeout = 2 * time.Minute
	}
	if req.Limit <= 0 {
		req.Limit = 4 << 20
	}
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	if req.Stall > 0 {
		var stop context.CancelCauseFunc
		ctx, stop = context.WithCancelCause(ctx)
		defer stop(nil)
		var last atomic.Int64
		last.Store(time.Now().UnixNano())
		tee := req.Output
		req.Output = func(b []byte) {
			last.Store(time.Now().UnixNano())
			if tee != nil {
				tee(b)
			}
		}
		go watchStall(ctx, &last, req.Stall, func() { stop(&StallError{Where: describe(req), Stall: req.Stall}) })
	}
	cmd, err := build(ctx, req, false)
	if err != nil {
		return Result{}, err
	}
	stdout := &limitedBuffer{limit: req.Limit}
	stderr := &limitedBuffer{limit: 64 << 10}
	if req.Output != nil {
		stdout.tee, stderr.tee = req.Output, req.Output
	}
	if req.Stdin != nil {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	configureHelperProcess(cmd)
	cmd.WaitDelay = 2 * time.Second
	runErr := cmd.Run()
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if runErr == nil {
		if stdout.Exceeded() {
			return res, fmt.Errorf("%s: output exceeded %d bytes", describe(req), req.Limit)
		}
		return res, nil
	}
	if ctx.Err() != nil {
		var stall *StallError
		if errors.As(context.Cause(ctx), &stall) {
			return res, stall
		}
		return res, fmt.Errorf("%s: %w", describe(req), ctx.Err())
	}
	diag := stderr.String()
	var exit *exec.ExitError
	if errors.As(runErr, &exit) {
		res.ExitCode = exit.ExitCode()
	} else {
		return res, fmt.Errorf("cannot start %s: %w", describe(req), runErr)
	}
	if req.Host != "" && res.ExitCode == 255 {
		if needsAuthentication(diag) {
			return res, &AuthRequiredError{Host: req.Host}
		}
		if t := classifySSHTransport(runErr, diag); t != nil {
			return res, t
		}
	}
	if req.Sudo && sudoWantsPassword(diag) {
		return res, ErrSudoPasswordRequired
	}
	return res, &CommandError{ExitCode: res.ExitCode, Stderr: tail(Sanitize(diag), 1200)}
}

// watchStall calls fire once when last (unix nanoseconds) is older than
// stall, and returns when ctx ends.
func watchStall(ctx context.Context, last *atomic.Int64, stall time.Duration, fire func()) {
	every := min(stall/4, time.Second)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if now.Sub(time.Unix(0, last.Load())) > stall {
				fire()
				return
			}
		}
	}
}

// InteractiveCommand returns a command that runs req.Script (as root when
// req.Sudo) attached to the caller's terminal: sudo and ssh prompt natively.
// Stdin of the request is not supported (the terminal owns stdin).
func InteractiveCommand(ctx context.Context, req Request) (*exec.Cmd, error) {
	if req.Stdin != nil {
		return nil, errors.New("interactive commands cannot take request stdin")
	}
	return build(ctx, req, true)
}

func build(ctx context.Context, req Request, interactive bool) (*exec.Cmd, error) {
	if strings.TrimSpace(req.Script) == "" {
		return nil, errors.New("empty script")
	}
	argv := []string{"sh", "-c", req.Script, "lazylongarm"}
	argv = append(argv, req.Args...)
	if req.Sudo {
		if interactive {
			argv = append([]string{"sudo", "--"}, argv...)
		} else {
			argv = append([]string{"sudo", "-n", "--"}, argv...)
		}
	}
	if req.Host == "" {
		return commandContext(ctx, argv[0], argv[1:]...), nil
	}
	if err := ValidateHost(req.Host); err != nil {
		return nil, err
	}
	base, err := sshArgsContext(ctx, req.Host)
	if err != nil {
		return nil, err
	}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = ShellQuote(a)
	}
	remote := strings.Join(quoted, " ")
	tty := "-T"
	if interactive {
		tty = "-tt"
		for i := range base {
			if base[i] == "BatchMode=yes" {
				base[i] = "BatchMode=no"
			}
		}
	}
	return commandContext(ctx, "ssh", append(base, tty, "--", req.Host, remote)...), nil
}

func sudoWantsPassword(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "a password is required") || strings.Contains(s, "a terminal is required") || strings.Contains(s, "no tty present") || strings.Contains(s, "askpass")
}

func describe(req Request) string {
	if req.Host == "" {
		return "local command"
	}
	return "SSH command on " + req.Host
}

// Sanitize removes terminal control sequences (CSI, OSC, other ESC
// sequences) and non-printing characters from untrusted output.
func Sanitize(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == 0x1b && i+1 < len(rs) && rs[i+1] == '[': // CSI: ESC [ params final(0x40-0x7e)
			i += 2
			for i < len(rs) && (rs[i] < 0x40 || rs[i] > 0x7e) {
				i++
			}
		case r == 0x1b && i+1 < len(rs) && (rs[i+1] == ']' || rs[i+1] == 'P' || rs[i+1] == '_' || rs[i+1] == '^'): // OSC/DCS/APC/PM: until BEL or ST
			i += 2
			for i < len(rs) && rs[i] != 0x07 && !(rs[i] == 0x1b && i+1 < len(rs) && rs[i+1] == '\\') {
				i++
			}
			if i < len(rs) && rs[i] == 0x1b {
				i++
			}
		case r == 0x1b:
			i++ // two-character escape
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
