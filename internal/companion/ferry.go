package companion

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// ferry (https://github.com/daviddwlee84/ferry) moves bulk data between two
// machines through a cloud relay (Baidu Pan, an rclone remote, a GitHub draft
// release) instead of the hub's relay. lazylongarm installs it and the
// backend CLI on fleet machines, hands the terminal over for backend logins,
// and writes the same job file on both sides of a transfer. It never reads,
// stores or copies backend credentials, and never starts a transfer.

// Ferry backends: the CLI each relay adapter drives on a machine.
const (
	BackendBaiduPCS = "baidupcs" // BaiduPCS-Go
	BackendRclone   = "rclone"
	BackendGH       = "gh" // GitHub CLI, for the ghrelease adapter
	// BackendRsync is rsync over ssh, sender to receiver (the rsync adapter).
	// It needs no login and comes from the system's package manager, so
	// `companion ferry setup` does not offer it.
	BackendRsync = "rsync"
)

// FerryBackends lists every backend lazylongarm knows, in display order.
var FerryBackends = []string{BackendBaiduPCS, BackendRclone, BackendGH}

// DefaultFerrySpec is what `uv tool install` gets when the config names no
// other source ([companions.ferry] install). It is pinned so every machine
// set up by the same lazylongarm gets the same ferry: sender and receiver
// share relay formats that may change between 0.x versions. ferry publishes
// no tags yet, so the pin is the 0.2.0 release commit.
const DefaultFerrySpec = "git+https://github.com/daviddwlee84/ferry@f8e5ccc2934ce34ba73725dc3b614d9c40b64368"

// MinFerryVersion is the oldest ferry `companion ferry pair` writes jobs for:
// 0.1.0 receivers trust unit keys and member paths from the relay.
const MinFerryVersion = "0.2.0"

// VersionAtLeast reports whether dotted version v >= minimum ("" is never).
func VersionAtLeast(v, minimum string) bool {
	if v == "" {
		return false
	}
	a, b := strings.Split(strings.TrimPrefix(v, "v"), "."), strings.Split(minimum, ".")
	for i := 0; i < len(a) || i < len(b); i++ {
		x, y := versionPart(a, i), versionPart(b, i)
		if x != y {
			return x > y
		}
	}
	return true
}

func versionPart(p []string, i int) int {
	if i >= len(p) {
		return 0
	}
	n := 0
	for _, r := range p[i] {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// BaiduPCSVersion is the BaiduPCS-Go release installed when a machine lacks it.
const BaiduPCSVersion = "v4.0.2"

// ValidBackend reports whether b is a known backend.
func ValidBackend(b string) error {
	if slices.Contains(FerryBackends, b) {
		return nil
	}
	return fmt.Errorf("unknown ferry backend %q (use %s)", b, strings.Join(FerryBackends, ", "))
}

// Relay adapters `companion ferry pair` writes jobs for. rsync pushes over
// ssh from the sender to the receiver: only worth it on a direct route (LAN,
// tailnet), since an ssh alias through `longarm proxy` takes the hub's relay
// when the workers have no direct path.
var FerryAdapters = []string{"baidupcs", "rclone", "ghrelease", "rsync"}

// AdapterBackend is the backend a relay adapter needs on both machines.
func AdapterBackend(adapter string) (string, error) {
	switch adapter {
	case "baidupcs":
		return BackendBaiduPCS, nil
	case "rclone":
		return BackendRclone, nil
	case "ghrelease":
		return BackendGH, nil
	case "rsync":
		return BackendRsync, nil
	}
	return "", fmt.Errorf("unknown relay adapter %q (use %s)", adapter, strings.Join(FerryAdapters, ", "))
}

// FerryStateScript reports ferry, uv and backend state as key=value lines.
// It is read-only, never prompts, and prints no account names or tokens:
// only whether each backend is signed in (BaiduPCS-Go: a non-zero uid), and
// rclone's remote names.
const FerryStateScript = RemotePath + `
kv() { printf '%s=%s\n' "$1" "$2"; }
fe=$(command -v ferry 2>/dev/null || true)
kv ferry "$fe"
if [ -n "$fe" ]; then kv ferry_version "$("$fe" --version 2>/dev/null </dev/null | awk '{print $NF}')"; fi
kv uv "$(command -v uv 2>/dev/null || true)"
bp=$(command -v BaiduPCS-Go 2>/dev/null || true)
if [ -z "$bp" ] && [ -x "$HOME/.local/bin/BaiduPCS-Go" ]; then bp="$HOME/.local/bin/BaiduPCS-Go"; fi
kv baidupcs "$bp"
if [ -n "$bp" ]; then
  uid=$("$bp" who 2>/dev/null </dev/null | sed -n 's/.*uid: *\([0-9][0-9]*\).*/\1/p' | head -n 1)
  if [ -n "$uid" ] && [ "$uid" != 0 ]; then kv baidupcs_login yes; else kv baidupcs_login no; fi
fi
rc=$(command -v rclone 2>/dev/null || true)
kv rclone "$rc"
if [ -n "$rc" ]; then kv rclone_remotes "$("$rc" listremotes 2>/dev/null </dev/null | tr '\n' ' ')"; fi
kv rsync "$(command -v rsync 2>/dev/null || true)"
gh=$(command -v gh 2>/dev/null || true)
kv gh "$gh"
if [ -n "$gh" ]; then
  if "$gh" auth status >/dev/null 2>&1 </dev/null; then kv gh_login yes; else kv gh_login no; fi
fi
kv done yes
`

// FerryState is what FerryStateScript found.
type FerryState struct {
	Ferry         string   `json:"ferry,omitempty"` // path, "" = not installed
	Version       string   `json:"version,omitempty"`
	UV            string   `json:"uv,omitempty"`
	BaiduPCS      string   `json:"baidupcs,omitempty"`
	BaiduPCSLogin bool     `json:"baidupcs_login,omitempty"`
	Rclone        string   `json:"rclone,omitempty"`
	RcloneRemotes []string `json:"rclone_remotes,omitempty"`
	GH            string   `json:"gh,omitempty"`
	GHLogin       bool     `json:"gh_login,omitempty"`
	Rsync         string   `json:"rsync,omitempty"`
}

// ParseFerryState decodes FerryStateScript output.
func ParseFerryState(out string) (FerryState, error) {
	var s FerryState
	done := false
	for line := range strings.SplitSeq(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "ferry":
			s.Ferry = v
		case "ferry_version":
			s.Version = v
		case "uv":
			s.UV = v
		case "baidupcs":
			s.BaiduPCS = v
		case "baidupcs_login":
			s.BaiduPCSLogin = v == "yes"
		case "rclone":
			s.Rclone = v
		case "rclone_remotes":
			s.RcloneRemotes = strings.Fields(v)
		case "gh":
			s.GH = v
		case "gh_login":
			s.GHLogin = v == "yes"
		case "rsync":
			s.Rsync = v
		case "done":
			done = v == "yes"
		}
	}
	if !done {
		return s, errors.New("incomplete ferry state report")
	}
	return s, nil
}

// Installed reports whether backend's CLI is on the machine.
func (s FerryState) Installed(backend string) bool {
	switch backend {
	case BackendBaiduPCS:
		return s.BaiduPCS != ""
	case BackendRclone:
		return s.Rclone != ""
	case BackendGH:
		return s.GH != ""
	case BackendRsync:
		return s.Rsync != ""
	}
	return false
}

// SignedIn reports whether backend is usable without a login (rclone: at
// least one remote is configured).
func (s FerryState) SignedIn(backend string) bool {
	switch backend {
	case BackendBaiduPCS:
		return s.BaiduPCSLogin
	case BackendRclone:
		return len(s.RcloneRemotes) > 0
	case BackendGH:
		return s.GHLogin
	case BackendRsync:
		return true
	}
	return false
}

// HasRcloneRemote reports whether rclone has the remote of an rclone
// adapter option such as "gdrive:" or "s3:bucket/relay".
func (s FerryState) HasRcloneRemote(remote string) bool {
	name, _, _ := strings.Cut(remote, ":")
	for _, r := range s.RcloneRemotes {
		if strings.TrimSuffix(r, ":") == name {
			return true
		}
	}
	return false
}

// BackendState is one backend's state in words: signed-in | login-needed |
// no-remotes (rclone without a configured remote) | not-installed.
func (s FerryState) BackendState(backend string) string {
	switch {
	case !s.Installed(backend):
		return "not-installed"
	case s.SignedIn(backend):
		return "signed-in"
	case backend == BackendRclone:
		return "no-remotes"
	}
	return "login-needed"
}

// SignedInBackends lists the backends usable without a login.
func (s FerryState) SignedInBackends() []string {
	var out []string
	for _, b := range FerryBackends {
		if s.Installed(b) && s.SignedIn(b) {
			out = append(out, b)
		}
	}
	return out
}

// Summary is a one-line state for tables: version, then each installed
// backend and whether it is signed in.
func (s FerryState) Summary() string {
	if s.Ferry == "" {
		return "not installed"
	}
	parts := []string{dashIfEmpty(s.Version)}
	for _, b := range FerryBackends {
		switch s.BackendState(b) {
		case "signed-in":
			parts = append(parts, b+" ok")
		case "login-needed":
			parts = append(parts, b+" login needed")
		case "no-remotes":
			parts = append(parts, b+" no remotes")
		}
	}
	return strings.Join(parts, " · ")
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// FerryInstallScript installs uv into ~/.local/bin when missing (Astral's
// installer, without editing shell profiles), then ferry from $1 with
// `uv tool install`. $2 = "upgrade" reinstalls from the source.
const FerryInstallScript = `set -e
` + RemotePath + `
spec=$1
if ! command -v uv >/dev/null 2>&1; then
  if command -v curl >/dev/null 2>&1; then
    curl -LsSf https://astral.sh/uv/install.sh | env UV_NO_MODIFY_PATH=1 sh
  elif command -v wget >/dev/null 2>&1; then
    wget -qO- https://astral.sh/uv/install.sh | env UV_NO_MODIFY_PATH=1 sh
  else
    echo "neither curl nor wget is available to install uv" >&2
    exit 1
  fi
fi
if [ "$2" = upgrade ]; then
  uv tool install --reinstall "$spec"
else
  uv tool install "$spec"
fi
ferry --version
`

// FerryInstallDisplay is FerryInstallScript in words for the Review.
func FerryInstallDisplay(spec string, upgrade, haveUV bool) string {
	var b strings.Builder
	if !haveUV {
		b.WriteString("install uv into ~/.local/bin (astral.sh/uv/install.sh, UV_NO_MODIFY_PATH=1)\n")
	}
	if upgrade {
		b.WriteString("uv tool install --reinstall ")
	} else {
		b.WriteString("uv tool install ")
	}
	b.WriteString(quoteArg(spec))
	return b.String()
}

// BaiduPCSAsset is the BaiduPCS-Go release archive name (without .zip) for
// a probed OS/arch, "" when there is none.
func BaiduPCSAsset(version, osName, arch string) string {
	suffix := map[string]string{
		"linux/amd64":  "linux-amd64",
		"linux/arm64":  "linux-arm64",
		"darwin/amd64": "darwin-osx-amd64",
		"darwin/arm64": "darwin-osx-arm64",
	}[osName+"/"+arch]
	if suffix == "" {
		return ""
	}
	return "BaiduPCS-Go-" + version + "-" + suffix
}

// BaiduPCSURL is the download URL of a release archive.
func BaiduPCSURL(version, asset string) string {
	return "https://github.com/qjfoidnh/BaiduPCS-Go/releases/download/" + version + "/" + asset + ".zip"
}

// BaiduPCSInstallScript downloads $1 (a release .zip URL), extracts the
// BaiduPCS-Go binary and installs it as ~/.local/bin/BaiduPCS-Go, where
// ferry looks by default.
const BaiduPCSInstallScript = `set -e
` + RemotePath + `
url=$1
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
if command -v curl >/dev/null 2>&1; then
  curl -fsSL -o "$tmp/b.zip" "$url"
elif command -v wget >/dev/null 2>&1; then
  wget -qO "$tmp/b.zip" "$url"
else
  echo "neither curl nor wget is available to download BaiduPCS-Go" >&2
  exit 1
fi
if command -v unzip >/dev/null 2>&1; then
  unzip -q "$tmp/b.zip" -d "$tmp/x"
elif command -v python3 >/dev/null 2>&1; then
  python3 -m zipfile -e "$tmp/b.zip" "$tmp/x"
elif command -v uv >/dev/null 2>&1; then
  uv run --no-project python -m zipfile -e "$tmp/b.zip" "$tmp/x"
else
  echo "no unzip, python3 or uv to extract BaiduPCS-Go" >&2
  exit 1
fi
bin=$(find "$tmp/x" -type f -name BaiduPCS-Go | head -n 1)
if [ -z "$bin" ]; then
  echo "the archive has no BaiduPCS-Go binary" >&2
  exit 1
fi
mkdir -p "$HOME/.local/bin"
cp "$bin" "$HOME/.local/bin/BaiduPCS-Go.new"
chmod 0755 "$HOME/.local/bin/BaiduPCS-Go.new"
mv "$HOME/.local/bin/BaiduPCS-Go.new" "$HOME/.local/bin/BaiduPCS-Go"
"$HOME/.local/bin/BaiduPCS-Go" --version </dev/null 2>/dev/null | head -n 1 || true
`

// LoginScript runs a backend's own interactive login in the user's terminal.
func LoginScript(backend string) (string, error) {
	switch backend {
	case BackendBaiduPCS:
		return RemotePath + `
bp=$(command -v BaiduPCS-Go 2>/dev/null || echo "$HOME/.local/bin/BaiduPCS-Go")
exec "$bp" login`, nil
	case BackendRclone:
		return RemotePath + "\nexec rclone config", nil
	case BackendGH:
		return RemotePath + "\nexec gh auth login", nil
	}
	return "", ValidBackend(backend)
}

// LoginCommand is the login a user can run by hand.
func LoginCommand(backend string) string {
	switch backend {
	case BackendBaiduPCS:
		return "BaiduPCS-Go login"
	case BackendRclone:
		return "rclone config"
	case BackendGH:
		return "gh auth login"
	}
	return ""
}

// ---- jobs ----

var jobName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidJobName checks a ferry job name (also its file name and state dir).
func ValidJobName(name string) error {
	if !jobName.MatchString(name) || len(name) > 64 {
		return fmt.Errorf("invalid ferry job name %q: letters, digits, '.', '_' and '-', starting with a letter or digit", name)
	}
	return nil
}

// JobNameFrom derives a job name from a source path's last element.
func JobNameFrom(p string) string {
	base := path.Base(strings.TrimRight(p, "/"))
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.TrimLeft(b.String(), "._-")
	if name == "" {
		return "transfer"
	}
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// ExpandPath makes p absolute on a machine whose home is home: "~" and
// "~/x" expand, relative paths are taken from the home directory.
func ExpandPath(p, home string) (string, error) {
	p = strings.TrimSpace(p)
	switch {
	case p == "":
		return "", errors.New("empty path")
	case p == "~":
		return path.Clean(home), nil
	case strings.HasPrefix(p, "~/"):
		return path.Join(home, p[2:]), nil
	case strings.HasPrefix(p, "~"):
		return "", fmt.Errorf("%s: ~user paths are not supported, write the absolute path", p)
	case strings.HasPrefix(p, "/"):
		return path.Clean(p), nil
	}
	if home == "" {
		return "", fmt.Errorf("%s: relative path, but the machine's home directory is unknown", p)
	}
	return path.Join(home, p), nil
}

// JobPath is where ferry's own wizard keeps a job on a machine with this
// home directory (~/.config/ferry/jobs/<name>.toml).
func JobPath(home, name string) string {
	return path.Join(home, ".config", "ferry", "jobs", name+".toml")
}

// FerryJob describes the one job file both machines of a pair get.
type FerryJob struct {
	Name         string
	SenderName   string // display names, for the header comment
	ReceiverName string
	Root         string // absolute, on the sender
	Glob         string // relative to Root
	Dest         string // absolute, on the receiver
	Extract      bool
	Adapter      string
	Prefix       string
	Options      map[string]string // adapter options (rclone remote, ghrelease repo/tag, rsync host/root)
	// ReceiverAdapter overrides the adapter on the receiving side
	// ([remote.sides.receiver]); rsync jobs read the landing directory
	// with "local" there.
	ReceiverAdapter string
	ReceiverOptions map[string]string
	DeleteSource    string // "" = never
	GeneratedBy     string // e.g. "lazylongarm v0.9.0"
	SourceSelected  string // what the glob selects, in words (comment only)
}

// tomlString is a TOML basic string (JSON escapes are valid TOML).
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return strings.NewReplacer(`<`, "<", `>`, ">", `&`, "&").Replace(string(b))
}

// Render writes the job as TOML. Sources stay put (delete_source = "never")
// and egress stays direct unless the job says otherwise.
func (j FerryJob) Render() (string, error) {
	if err := ValidJobName(j.Name); err != nil {
		return "", err
	}
	if _, err := AdapterBackend(j.Adapter); err != nil {
		return "", err
	}
	if !strings.HasPrefix(j.Root, "/") || !strings.HasPrefix(j.Dest, "/") {
		return "", errors.New("source and destination must be absolute paths")
	}
	if j.Glob == "" || strings.HasPrefix(j.Glob, "/") || strings.Contains(j.Glob, "..") {
		return "", fmt.Errorf("invalid unit glob %q: a pattern relative to the source root", j.Glob)
	}
	del := j.DeleteSource
	if del == "" {
		del = "never"
	}
	switch del {
	case "never", "after_remote_size", "after_receiver_ack":
	default:
		return "", fmt.Errorf("invalid delete_source %q (never, after_remote_size, after_receiver_ack)", del)
	}
	var b strings.Builder
	by := j.GeneratedBy
	if by == "" {
		by = "lazylongarm"
	}
	fmt.Fprintf(&b, "# ferry job written by %s (companion ferry pair).\n", by)
	fmt.Fprintf(&b, "# The same file is on both machines: sender %s, receiver %s.\n", dashIfEmpty(j.SenderName), dashIfEmpty(j.ReceiverName))
	b.WriteString("# Backend logins live in each machine's backend CLI, never here.\n")
	fmt.Fprintf(&b, "name = %s\n\n", tomlString(j.Name))
	b.WriteString("[units]\n")
	fmt.Fprintf(&b, "root = %s\n", tomlString(j.Root))
	if j.SourceSelected != "" {
		fmt.Fprintf(&b, "glob = %s  # %s\n\n", tomlString(j.Glob), j.SourceSelected)
	} else {
		fmt.Fprintf(&b, "glob = %s\n\n", tomlString(j.Glob))
	}
	b.WriteString("[sender]\n")
	fmt.Fprintf(&b, "delete_source = %s\n\n", tomlString(del))
	b.WriteString("[receiver]\n")
	fmt.Fprintf(&b, "dest = %s\n", tomlString(j.Dest))
	fmt.Fprintf(&b, "extract = %t\n\n", j.Extract)
	b.WriteString("[remote]\n")
	fmt.Fprintf(&b, "adapter = %s\n", tomlString(j.Adapter))
	fmt.Fprintf(&b, "prefix = %s\n", tomlString(j.Prefix))
	b.WriteString("egress = \"direct\"\n")
	if len(j.Options) > 0 {
		fmt.Fprintf(&b, "options = { %s }\n", inlineTable(j.Options))
	}
	if j.ReceiverAdapter != "" {
		b.WriteString("\n[remote.sides.receiver]\n")
		fmt.Fprintf(&b, "adapter = %s\n", tomlString(j.ReceiverAdapter))
		if len(j.ReceiverOptions) > 0 {
			fmt.Fprintf(&b, "options = { %s }\n", inlineTable(j.ReceiverOptions))
		}
	}
	return b.String(), nil
}

func inlineTable(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + " = " + tomlString(m[k])
	}
	return strings.Join(parts, ", ")
}

// SSHRouteScript prints how ssh on a machine resolves host $1 (ssh -G):
// hostname, proxycommand and proxyjump lines.
const SSHRouteScript = `ssh -G "$1" 2>/dev/null </dev/null | awk '$1=="hostname"||$1=="proxycommand"||$1=="proxyjump"'
`

// SSHRoute is what ssh -G reported.
type SSHRoute struct {
	HostName     string `json:"hostname,omitempty"`
	ProxyCommand string `json:"proxycommand,omitempty"`
	ProxyJump    string `json:"proxyjump,omitempty"`
}

// ParseSSHRoute reads SSHRouteScript output ("none" values are dropped).
func ParseSSHRoute(out string) SSHRoute {
	var r SSHRoute
	for line := range strings.SplitSeq(out, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), " ")
		v = strings.TrimSpace(v)
		if v == "none" {
			v = ""
		}
		switch k {
		case "hostname":
			r.HostName = v
		case "proxycommand":
			r.ProxyCommand = v
		case "proxyjump":
			r.ProxyJump = v
		}
	}
	return r
}

// ThroughLongarm reports whether the route is Longarm's mTLS channel
// (ProxyCommand longarm proxy <worker>), which falls back to the hub's
// relay when the two machines have no direct path.
func (r SSHRoute) ThroughLongarm(host string) bool {
	return strings.HasPrefix(host, LongarmHostPrefix) || (strings.Contains(r.ProxyCommand, "longarm") && strings.Contains(r.ProxyCommand, "proxy"))
}

// JobFileScript compares (or, with $2 = write, writes) the job on stdin with
// the file at $1. It prints state=missing|same|differs|written and never
// replaces a file whose content differs.
const JobFileScript = `set -e
p=$1
if [ -e "$p" ] || [ -L "$p" ]; then
  if [ -f "$p" ] && cmp -s "$p" -; then echo state=same; else echo state=differs; fi
  exit 0
fi
if [ "$2" != write ]; then
  cat >/dev/null
  echo state=missing
  exit 0
fi
mkdir -p "$(dirname "$p")"
tmp="$p.lazylongarm.$$"
cat > "$tmp"
chmod 0644 "$tmp"
mv "$tmp" "$p"
echo state=written
`

// ParseJobFileState reads JobFileScript output.
func ParseJobFileState(out string) (string, error) {
	for line := range strings.SplitSeq(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "state="); ok {
			return v, nil
		}
	}
	return "", errors.New("no job file state reported")
}

// PathKindScript prints kind=dir|file|missing|other for $1.
const PathKindScript = `p=$1
if [ -d "$p" ]; then echo kind=dir
elif [ -f "$p" ]; then echo kind=file
elif [ -e "$p" ]; then echo kind=other
else echo kind=missing
fi
`

// ParsePathKind reads PathKindScript output.
func ParsePathKind(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "kind="); ok {
			return v
		}
	}
	return ""
}

// FerryRunScript runs ferry with the positional parameters, the same PATH as
// the other companion scripts.
const FerryRunScript = RemotePath + `
exec ferry "$@" </dev/null`

// FerryPlanUnits counts the units in `ferry plan --json` output.
func FerryPlanUnits(out []byte) (int, error) {
	var v struct {
		Units []json.RawMessage `json:"units"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return 0, fmt.Errorf("ferry plan output: %w", err)
	}
	return len(v.Units), nil
}
