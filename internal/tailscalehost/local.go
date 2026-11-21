package tailscalehost

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// ErrNoCLI means no tailscale CLI was found on this machine.
var ErrNoCLI = errors.New("Tailscale is not installed on this machine")

// macAppCLI is the CLI inside the macOS app when `tailscale` is not on PATH.
const macAppCLI = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"

// LocalStatus runs `tailscale status --json` on this machine.
func LocalStatus(ctx context.Context) (Status, error) {
	bin, err := exec.LookPath("tailscale")
	if err != nil && runtime.GOOS == "darwin" {
		bin, err = exec.LookPath(macAppCLI)
	}
	if err != nil {
		return Status{}, ErrNoCLI
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "status", "--json").Output()
	if len(out) > 0 {
		if st, perr := ParseStatus(out); perr == nil {
			return st, nil
		}
	}
	if err == nil {
		err = errors.New("no status output")
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		err = errors.New(strings.TrimSpace(firstLine(string(ee.Stderr))))
	}
	return Status{}, fmt.Errorf("tailscale status: %w", err)
}

// LocalCheck reports whether this machine can open rawURL on its tailnet;
// reason says why not.
func LocalCheck(ctx context.Context, rawURL string) (bool, string) {
	st, err := LocalStatus(ctx)
	return CheckStatus(st, err, rawURL)
}

// CheckStatus is LocalCheck on a status already read: Tailscale must be
// running here, on the tailnet whose MagicDNS suffix the URL's host ends with.
func CheckStatus(st Status, err error, rawURL string) (bool, string) {
	if errors.Is(err, ErrNoCLI) {
		return false, err.Error()
	}
	if err != nil {
		return false, "Tailscale on this machine did not answer (" + err.Error() + ")"
	}
	if !st.Running() {
		return false, "Tailscale on this machine is " + orUnknown(st.BackendState) + ", not connected"
	}
	u, perr := url.Parse(rawURL)
	if perr != nil || u.Hostname() == "" {
		return true, ""
	}
	suffix := strings.TrimSuffix(st.MagicDNSSuffix, ".")
	if suffix == "" {
		// Older clients: the suffix is this node's name minus its first label.
		if _, rest, ok := strings.Cut(st.DNSName(), "."); ok {
			suffix = rest
		}
	}
	if suffix != "" && !strings.HasSuffix(u.Hostname(), "."+suffix) {
		return false, "this machine is on tailnet " + suffix + ", not the panel's"
	}
	return true, ""
}

func orUnknown(s string) string {
	if s == "" {
		return "in an unknown state"
	}
	return s
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}
