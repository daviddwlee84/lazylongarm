package connection

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Push timing. A copy fails when the receiving side sees no new bytes for
// PushStall, or when it runs longer than PushTimeout(size). Variables so tests
// can shorten them.
var (
	PushStall   = 60 * time.Second
	pushTick    = 5        // seconds between progress reports from the receiving side
	pushMinRate = 16 << 10 // bytes/s the overall deadline allows for
)

// PushTimeout is the overall deadline for pushing size bytes: two minutes plus
// the size at 16 KiB/s (about 45 minutes for a 40 MB binary). The stall
// timeout is what normally ends a dead copy; this only bounds a trickle.
func PushTimeout(size int) time.Duration {
	return 2*time.Minute + time.Duration(size/pushMinRate)*time.Second
}

// PushOptions tunes PushFile.
type PushOptions struct {
	// SHA256, when set, must match the received copy before it replaces dest.
	SHA256 string
	// Progress receives the byte count the receiving side has written, about
	// every five seconds while it grows.
	Progress func(received, total int64)
}

const progressPrefix = "lazylongarm-progress "

// pushScript receives the file on stdin into a temporary sibling of dest,
// reports its growing size on stdout (the stall timeout's heartbeat), checks
// the size (a dropped connection ends cat with EOF, not an error) and the
// optional sha256, and only then chmods and renames it into place.
const pushScript = `set -eu
dest="$1"; mode="$2"; size="$3"; want="$4"; tick="$5"
case "$dest" in "\$HOME/"*) dest="$HOME/${dest#\$HOME/}";; esac
dir=$(dirname -- "$dest")
mkdir -p -- "$dir"
tmp="$dest.lazylongarm.$$"
umask 077
: > "$tmp"
ticker=
trap 'rm -f -- "$tmp"; if [ -n "$ticker" ]; then kill "$ticker" 2>/dev/null || true; fi' EXIT
(
  last=0
  while sleep "$tick" >/dev/null 2>&1; do
    n=$(wc -c < "$tmp" | tr -d ' ') || exit 0
    if [ "$n" != "$last" ]; then echo "lazylongarm-progress $n"; last=$n; fi
  done
) </dev/null &
ticker=$!
cat > "$tmp"
kill "$ticker" 2>/dev/null || true
wait "$ticker" 2>/dev/null || true
ticker=
n=$(wc -c < "$tmp" | tr -d ' ')
if [ "$n" != "$size" ]; then echo "received $n of $size bytes; nothing installed" >&2; exit 91; fi
if [ -n "$want" ]; then
  if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$tmp" | cut -d' ' -f1); else got=$(shasum -a 256 "$tmp" | cut -d' ' -f1); fi
  if [ "$got" != "$want" ]; then echo "checksum mismatch: received sha256 $got, expected $want; nothing installed" >&2; exit 91; fi
fi
chmod "$mode" "$tmp"
mv -f -- "$tmp" "$dest"`

// PushFile copies data to dest on host (or locally when host == "") through
// ssh stdin: written to a temporary sibling, checked (size, optional sha256),
// chmod'ed, then renamed, so an interrupted copy never replaces dest. It fails
// after PushStall without progress and after PushTimeout(len(data)) overall.
// dest may start with "$HOME/"; it is expanded by the remote shell.
func PushFile(ctx context.Context, host string, data []byte, dest, mode string, opt PushOptions) error {
	if err := validDest(dest); err != nil {
		return err
	}
	if opt.SHA256 != "" && !sha256RE.MatchString(opt.SHA256) {
		return errors.New("invalid sha256")
	}
	total := int64(len(data))
	var mu sync.Mutex
	var pending []byte
	req := Request{
		Host: host, Script: pushScript,
		Args:    []string{dest, mode, strconv.Itoa(len(data)), opt.SHA256, strconv.Itoa(pushTick)},
		Stdin:   data,
		Timeout: PushTimeout(len(data)),
		Stall:   PushStall,
		Output: func(b []byte) {
			mu.Lock()
			defer mu.Unlock()
			pending = append(pending, b...)
			for {
				i := strings.IndexByte(string(pending), '\n')
				if i < 0 {
					return
				}
				line := string(pending[:i])
				pending = pending[i+1:]
				if n, ok := strings.CutPrefix(line, progressPrefix); ok && opt.Progress != nil {
					if v, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); err == nil {
						opt.Progress(v, total)
					}
				}
			}
		},
	}
	_, err := Run(ctx, req)
	var stall *StallError
	if errors.As(err, &stall) {
		return fmt.Errorf("copying to %s stalled: no bytes arrived for %s (the connection is dead or stuck; nothing was installed)", where(host), stall.Stall)
	}
	return err
}

// FetchOptions bounds a download on the remote machine.
type FetchOptions struct {
	ConnectTimeout time.Duration // curl --connect-timeout / GNU wget --connect-timeout
	SpeedLimit     int           // bytes/s; curl aborts when slower than this for SpeedTime
	SpeedTime      time.Duration // curl --speed-time; wget's read (idle) timeout
	Timeout        time.Duration // overall ceiling of the ssh command
}

// FetchError is a failed remote download, by kind.
type FetchError struct {
	Kind   string // FetchDownload | FetchChecksum | FetchTool
	Detail string // the remote side's message (sanitized)
}

// FetchError kinds.
const (
	FetchDownload = "download" // curl/wget failed (no route, DNS, timeout, HTTP error, too slow)
	FetchChecksum = "checksum" // downloaded bytes differ from the expected sha256; nothing installed
	FetchTool     = "tool"     // no curl/wget, or no sha256sum/shasum
)

func (e *FetchError) Error() string {
	switch e.Kind {
	case FetchChecksum:
		return "downloaded file failed verification: " + e.Detail
	case FetchTool:
		return "cannot download there: " + e.Detail
	}
	return "download failed: " + e.Detail
}

const fetchScript = `set -eu
url="$1"; want="$2"; dest="$3"; mode="$4"; ct="$5"; sl="$6"; st="$7"
case "$dest" in "\$HOME/"*) dest="$HOME/${dest#\$HOME/}";; esac
dir=$(dirname -- "$dest")
mkdir -p -- "$dir"
tmp="$dest.lazylongarm.$$"
umask 077
trap 'rm -f -- "$tmp"' EXIT
if command -v curl >/dev/null 2>&1; then
  curl -fsSL --proto =https --proto-redir =https --connect-timeout "$ct" --speed-limit "$sl" --speed-time "$st" -o "$tmp" "$url" || exit 90
elif command -v wget >/dev/null 2>&1; then
  if wget --version 2>/dev/null | grep -q 'GNU Wget'; then
    wget -q --tries=1 --connect-timeout="$ct" --read-timeout="$st" -O "$tmp" "$url" || exit 90
  else
    wget -q -T "$st" -O "$tmp" "$url" || exit 90
  fi
else
  echo "neither curl nor wget is installed" >&2; exit 92
fi
if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$tmp" | cut -d' ' -f1)
elif command -v shasum >/dev/null 2>&1; then got=$(shasum -a 256 "$tmp" | cut -d' ' -f1)
else echo "neither sha256sum nor shasum is installed" >&2; exit 92; fi
if [ "$got" != "$want" ]; then echo "downloaded sha256 $got, SHA256SUMS says $want; nothing installed" >&2; exit 91; fi
chmod "$mode" "$tmp"
mv -f -- "$tmp" "$dest"`

var sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// FetchFile makes host (or this machine when host == "") download url with
// curl (wget when curl is missing) into a temporary sibling of dest, checks it
// against sha256 (the value the caller trusts, e.g. from a verified
// SHA256SUMS), then chmods and renames it. A mismatch deletes the download and
// installs nothing (*FetchError of kind FetchChecksum).
func FetchFile(ctx context.Context, host, url, sha256, dest, mode string, opt FetchOptions) error {
	if err := validDest(dest); err != nil {
		return err
	}
	if !strings.HasPrefix(url, "https://") || strings.ContainsAny(url, " \t\x00\r\n'\"`;|&<>$\\") {
		return fmt.Errorf("invalid download URL %q", url)
	}
	if !sha256RE.MatchString(sha256) {
		return errors.New("invalid sha256")
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 30 * time.Minute
	}
	req := Request{
		Host: host, Script: fetchScript, Timeout: opt.Timeout,
		Args: []string{url, sha256, dest, mode, seconds(opt.ConnectTimeout), strconv.Itoa(max(opt.SpeedLimit, 1)), seconds(opt.SpeedTime)},
	}
	_, err := Run(ctx, req)
	var ce *CommandError
	if errors.As(err, &ce) {
		detail := strings.TrimSpace(ce.Stderr)
		switch ce.ExitCode {
		case 90:
			if detail == "" {
				detail = "curl/wget exited with an error"
			}
			return &FetchError{Kind: FetchDownload, Detail: detail}
		case 91:
			return &FetchError{Kind: FetchChecksum, Detail: detail}
		case 92:
			return &FetchError{Kind: FetchTool, Detail: detail}
		}
	}
	return err
}

// FetchCommand is the Review form of FetchFile's download, verification and
// install for one file.
func FetchCommand(url, dest, mode string, opt FetchOptions) string {
	return fmt.Sprintf("curl -fsSL --proto =https --proto-redir =https --connect-timeout %s --speed-limit %d --speed-time %s -o %s.lazylongarm.$$ %s\n", seconds(opt.ConnectTimeout), max(opt.SpeedLimit, 1), seconds(opt.SpeedTime), dest, url) +
		fmt.Sprintf("  (wget --tries=1 --connect-timeout=%s --read-timeout=%s when curl is missing)\n", seconds(opt.ConnectTimeout), seconds(opt.SpeedTime)) +
		fmt.Sprintf("sha256 must equal the SHA256SUMS entry → chmod %s, rename to %s; mismatch: delete it, install nothing", mode, dest)
}

func seconds(d time.Duration) string {
	s := int(d / time.Second)
	if s < 1 {
		s = 1
	}
	return strconv.Itoa(s)
}

func validDest(dest string) error {
	if dest == "" || strings.ContainsAny(dest, "\x00\r\n'\"`;|&<>") {
		return errors.New("invalid destination path")
	}
	return nil
}

func where(host string) string {
	if host == "" {
		return "this machine"
	}
	return host
}
