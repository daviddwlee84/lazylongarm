package connection

import (
	"errors"
	"os/exec"
	"strings"
)

// SSHTransportError classifies known transport failures without retaining raw
// stderr, host names or command text, any of which may contain private data.
type SSHTransportError struct{ Kind string }

func (e *SSHTransportError) Error() string {
	switch e.Kind {
	case "timeout":
		return "SSH connection timed out; inspect the route, proxy/TUN routing and SSH ingress firewall"
	case "refused":
		return "SSH connection was refused; inspect the SSH listener, selected port and firewall"
	case "unreachable":
		return "SSH host has no reachable network route; inspect local routing, proxy/TUN routing and the remote network"
	case "resolve":
		return "SSH hostname could not be resolved; inspect the SSH alias and DNS"
	case "closed":
		return "SSH connection closed before the command completed; inspect transport and SSH service logs"
	default:
		return "SSH transport did not complete; inspect the host's SSH route and firewall"
	}
}

func classifySSHTransport(err error, diagnostic string) *SSHTransportError {
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 255 {
		return nil
	}
	message := strings.ToLower(diagnostic)
	switch {
	case strings.Contains(message, "could not resolve hostname"):
		return &SSHTransportError{Kind: "resolve"}
	case strings.Contains(message, "connection timed out during banner exchange"):
		return &SSHTransportError{Kind: "timeout"}
	case strings.Contains(message, "connect to host") && (strings.Contains(message, "connection timed out") || strings.Contains(message, "operation timed out")):
		return &SSHTransportError{Kind: "timeout"}
	case strings.Contains(message, "connect to host") && strings.Contains(message, "connection refused"):
		return &SSHTransportError{Kind: "refused"}
	case strings.Contains(message, "connect to host") && (strings.Contains(message, "no route to host") || strings.Contains(message, "network is unreachable")):
		return &SSHTransportError{Kind: "unreachable"}
	case strings.Contains(message, "connection closed by") || strings.Contains(message, "connection reset by") || strings.Contains(message, "kex_exchange_identification:"):
		return &SSHTransportError{Kind: "closed"}
	}
	return nil
}
