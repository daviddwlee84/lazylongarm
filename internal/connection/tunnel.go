package connection

import (
	"context"
	"errors"
	"io"
	"net"
	"os/exec"
	"sync"
	"time"
)

// Tunnel is an app-owned `ssh -N -L` child. Its lifetime is the caller's:
// Close stops it. It never attaches a forwarding to a shared master, so
// killing the child always removes the forwarding.
type Tunnel struct {
	Address string // local 127.0.0.1:port
	cmd     *exec.Cmd
	done    chan struct{}
	once    sync.Once
	cancel  context.CancelFunc
}

// Done is closed when the ssh child exits.
func (t *Tunnel) Done() <-chan struct{} { return t.done }

// Close stops the tunnel and waits briefly for the child to exit.
func (t *Tunnel) Close() error {
	t.once.Do(func() {
		t.cancel()
		select {
		case <-t.done:
		case <-time.After(2 * time.Second):
			if t.cmd.Process != nil {
				_ = t.cmd.Process.Kill()
			}
			<-t.done
		}
	})
	return nil
}

// OpenTunnel forwards localPort (0 = any free port) on 127.0.0.1 to
// remoteHostPort as seen from host.
func OpenTunnel(ctx context.Context, host string, localPort int, remoteHostPort string) (*Tunnel, error) {
	if err := ValidateHost(host); err != nil {
		return nil, err
	}
	if _, err := resolveMultiplexPolicy(ctx, host); err != nil {
		return nil, err
	}
	address := net.JoinHostPort("127.0.0.1", itoa(localPort))
	if localPort == 0 {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		address = listener.Addr().String()
		_ = listener.Close()
	}
	forward := address + ":" + remoteHostPort
	args := append(batchSSHArgs("none"), "-o", "ExitOnForwardFailure=yes", "-o", "ForwardAgent=no", "-N", "-T", "-L", forward, "--", host)
	tctx, cancel := context.WithCancel(context.Background())
	cmd := commandContext(tctx, "ssh", args...)
	diagnostic := &limitedBuffer{limit: 8192}
	cmd.Stderr, cmd.Stdout = diagnostic, io.Discard
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, errors.New("cannot start ssh; ensure OpenSSH is installed")
	}
	t := &Tunnel{Address: address, cmd: cmd, done: make(chan struct{}), cancel: cancel}
	go func() { _ = cmd.Wait(); close(t.done) }()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = t.Close()
			return nil, ctx.Err()
		case <-deadline.C:
			_ = t.Close()
			return nil, errors.New("SSH tunnel was not ready within 10 seconds")
		case <-t.done:
			cancel()
			if needsAuthentication(diagnostic.String()) {
				return nil, &AuthRequiredError{Host: host}
			}
			return nil, errors.New("SSH tunnel exited before becoming ready: " + tail(Sanitize(diagnostic.String()), 300))
		case <-tick.C:
			conn, err := net.DialTimeout("tcp", address, 80*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				return t, nil
			}
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
