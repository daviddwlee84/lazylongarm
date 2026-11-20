// Package connection runs fixed commands locally or on an SSH host through the
// system OpenSSH client. It never parses SSH configuration for authority (only
// `ssh -G` multiplexing fields), never collects passwords, and turns prompts
// into typed errors so a terminal UI can hand the real terminal to ssh/sudo.
//
// Adapted from lazyclash internal/connection (ssh.go, multiplex.go).
package connection

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var commandContext = exec.CommandContext

// AuthRequiredError is a terminal handoff request, not an authentication prompt.
// Background work uses BatchMode, so no hidden process competes for TUI input.
type AuthRequiredError struct{ Host string }

func (e *AuthRequiredError) Error() string {
	return fmt.Sprintf("SSH authentication or host-key approval required for %s; authenticate in a terminal (ssh %s) and retry", e.Host, e.Host)
}

// IsAuthRequired reports whether err asks for an interactive SSH login.
func IsAuthRequired(err error) bool { var e *AuthRequiredError; return errors.As(err, &e) }

type authentication struct{ dir, path string }

var authentications = struct {
	sync.Mutex
	hosts map[string]authentication
}{hosts: map[string]authentication{}}

// AuthenticateCommand uses the user's persistent multiplexing policy when
// available; otherwise it creates a private, invocation-scoped 60s master.
// Run it on the real terminal, then retry the operation once. Configured
// sockets are never app-owned and are never closed by this package.
func AuthenticateCommand(ctx context.Context, host string) (*exec.Cmd, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ValidateHost(host); err != nil {
		return nil, err
	}
	policy, err := resolveMultiplexPolicy(ctx, host)
	if err != nil {
		return nil, err
	}
	useConfigured, master := policy.persistent(), policy.master
	if policy.path != "" {
		alive, err := configuredMasterAlive(ctx, host, policy.path)
		if err != nil {
			return nil, err
		}
		if alive {
			useConfigured, master = true, "no"
		}
	}
	authentications.Lock()
	a, ok := authentications.hosts[host]
	if !ok && useConfigured {
		authentications.Unlock()
		return commandContext(ctx, "ssh", "-o", "BatchMode=no", "-o", "ConnectTimeout=15", "-o", "ClearAllForwardings=yes", "-o", "ControlMaster="+master, "-o", "ControlPersist="+policy.persist, "-o", "ControlPath="+controlSocketArgument(policy.path), "-T", "--", host, "true"), nil
	}
	if !ok {
		dir, err := os.MkdirTemp("", "lazylongarm-ssh-")
		if err != nil {
			authentications.Unlock()
			return nil, err
		}
		a = authentication{dir: dir, path: filepath.Join(dir, "control")}
		authentications.hosts[host] = a
	}
	authentications.Unlock()
	return commandContext(ctx, "ssh", "-o", "BatchMode=no", "-o", "ConnectTimeout=15", "-o", "ClearAllForwardings=yes", "-o", "ControlMaster=auto", "-o", "ControlPersist=120", "-o", "ControlPath="+controlSocketArgument(a.path), "-T", "--", host, "true"), nil
}

// CloseAuthentications closes only the private masters this process created.
func CloseAuthentications() error {
	authentications.Lock()
	owned := authentications.hosts
	authentications.hosts = map[string]authentication{}
	authentications.Unlock()
	clearMultiplexPolicies()
	for host, a := range owned {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = commandContext(ctx, "ssh", append(masterControlArgs(a.path, "exit"), "--", host)...).Run()
		cancel()
		_ = os.RemoveAll(a.dir)
	}
	return nil
}

// ValidateHost accepts an OpenSSH destination (alias, user@host, host:port is
// not accepted; use an alias). It rejects option injection and shell syntax.
func ValidateHost(host string) error {
	if host == "" {
		return errors.New("SSH host is required")
	}
	if len(host) > 255 || strings.HasPrefix(host, "-") {
		return fmt.Errorf("invalid SSH host %q", host)
	}
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("._-@[]:", r):
		default:
			return fmt.Errorf("invalid SSH host %q: use an alias from ~/.ssh/config or user@host", host)
		}
	}
	return nil
}

func batchSSHArgs(controlPath string) []string {
	args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=8", "-o", "StrictHostKeyChecking=yes", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=2", "-o", "ControlMaster=no"}
	if controlPath == "" {
		controlPath = "none"
	}
	return append(args, "-o", "ControlPath="+controlSocketArgument(controlPath))
}

func sshArgs(host string) []string {
	var policy multiplexPolicy
	multiplexPolicies.Lock()
	if entry := multiplexPolicies.entries[host]; entry != nil && entry.ready == nil {
		policy = entry.policy
	}
	multiplexPolicies.Unlock()
	authentications.Lock()
	a, ok := authentications.hosts[host]
	authentications.Unlock()
	path := policy.path
	if ok {
		path = a.path
	}
	return append(batchSSHArgs(path), "-o", "ClearAllForwardings=yes", "-o", "ForwardAgent=no")
}

// sshArgsContext resolves (and caches) the host's multiplexing policy, then
// returns batch-mode arguments that reuse a live master when one exists. An
// unreachable socket with ControlMaster=no makes OpenSSH connect directly.
func sshArgsContext(ctx context.Context, host string) ([]string, error) {
	if _, err := resolveMultiplexPolicy(ctx, host); err != nil {
		return nil, err
	}
	return sshArgs(host), nil
}

// ShellQuote single-quotes s for a POSIX shell.
func ShellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

type limitedBuffer struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	limit    int
	exceeded bool
	tee      func([]byte)
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tee != nil {
		b.tee(p)
	}
	if b.buffer.Len()+len(p) > b.limit {
		b.exceeded = true
		room := b.limit - b.buffer.Len()
		if room > 0 {
			b.buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.buffer.Write(p)
}
func (b *limitedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buffer.Bytes()...)
}
func (b *limitedBuffer) String() string { return string(b.Bytes()) }
func (b *limitedBuffer) Exceeded() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.exceeded }

func needsAuthentication(stderr string) bool {
	s := strings.ToLower(stderr)
	if strings.Contains(s, "host key verification failed") || strings.Contains(s, "no supported authentication methods") || strings.Contains(s, "read_passphrase") || strings.Contains(s, "authenticity of host") {
		return true
	}
	for _, line := range strings.Split(s, "\n") {
		_, tail, ok := strings.Cut(line, "permission denied")
		if !ok {
			continue
		}
		tail = strings.TrimSpace(tail)
		if strings.HasPrefix(tail, ", please try again") {
			return true
		}
		if strings.HasPrefix(tail, "(") {
			methods, _, closed := strings.Cut(tail[1:], ")")
			if !closed {
				continue
			}
			for _, method := range strings.Split(methods, ",") {
				switch strings.TrimSpace(method) {
				case "publickey", "password", "keyboard-interactive", "hostbased", "gssapi-with-mic", "gssapi-keyex":
					return true
				}
			}
		}
	}
	return false
}

func missingControlMaster(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "control socket") && (strings.Contains(s, "no such file") || strings.Contains(s, "connection refused") || strings.Contains(s, "connection reset"))
}
