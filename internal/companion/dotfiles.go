package companion

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/daviddwlee84/lazylongarm/internal/connection"
)

// RemotePath is prepended to PATH in scripts run on a machine: a
// non-interactive ssh shell often lacks the directories chezmoi installs to
// (~/.local/bin, or ./bin, which is get.chezmoi.io's default).
const RemotePath = `PATH="$HOME/.local/bin:$HOME/bin:/usr/local/bin:/opt/homebrew/bin:$PATH"; export PATH`

// StateScript reports a machine's chezmoi state as key=value lines. It is
// read-only and never prompts.
const StateScript = RemotePath + `
kv() { printf '%s=%s\n' "$1" "$2"; }
cz=$(command -v chezmoi 2>/dev/null || true)
kv chezmoi "$cz"
if [ -n "$cz" ]; then
  src=$("$cz" source-path 2>/dev/null || true)
  if [ -n "$src" ] && git -C "$src" rev-parse --git-dir >/dev/null 2>&1; then
    kv source "$src"
    kv origin "$(git -C "$src" remote get-url origin 2>/dev/null || true)"
    kv rev "$(git -C "$src" rev-parse --short HEAD 2>/dev/null || true)"
  fi
fi
kv done yes
`

// DotfilesState is what StateScript found.
type DotfilesState struct {
	Chezmoi string `json:"chezmoi,omitempty"` // path, "" = not installed
	Source  string `json:"source,omitempty"`  // source directory (a git checkout)
	Origin  string `json:"origin,omitempty"`  // remote URL, credentials removed
	Rev     string `json:"rev,omitempty"`
}

// Initialized reports whether chezmoi has a source checkout.
func (s DotfilesState) Initialized() bool { return s.Source != "" }

// ParseState decodes StateScript output.
func ParseState(out string) (DotfilesState, error) {
	var s DotfilesState
	done := false
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "chezmoi":
			s.Chezmoi = v
		case "source":
			s.Source = v
		case "origin":
			s.Origin = StripCredentials(v)
		case "rev":
			s.Rev = v
		case "done":
			done = true
		}
	}
	if !done {
		return s, errors.New("dotfiles state check did not complete")
	}
	return s, nil
}

// StripCredentials removes user:password@ from an http(s) URL so a token in
// a remote URL never reaches output or the config file.
func StripCredentials(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil || (u.Scheme != "https" && u.Scheme != "http") {
		return raw
	}
	u.User = nil
	return u.String()
}

var scpLike = regexp.MustCompile(`^(?:[\w.-]+@)?([\w.-]+):(.+)$`)

// NormalizeRepo reduces a repository reference to host/path for comparison.
// It accepts chezmoi's short forms ("user" → github.com/user/dotfiles,
// "user/repo" → github.com/user/repo), https/ssh URLs and scp-like
// git@host:path.
func NormalizeRepo(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	clean := func(host, path string) string {
		path = strings.Trim(path, "/")
		path = strings.TrimSuffix(path, ".git")
		return strings.ToLower(host) + "/" + path
	}
	if strings.Contains(s, "://") {
		if u, err := url.Parse(s); err == nil && u.Host != "" {
			return clean(u.Hostname(), u.Path)
		}
		return s
	}
	if m := scpLike.FindStringSubmatch(s); m != nil && strings.Contains(m[1], ".") {
		return clean(m[1], m[2])
	}
	parts := strings.Split(strings.Trim(s, "/"), "/")
	switch {
	case len(parts) == 1:
		return clean("github.com", parts[0]+"/dotfiles")
	case len(parts) == 2:
		return clean("github.com", s)
	case strings.Contains(parts[0], "."):
		return clean(parts[0], strings.Join(parts[1:], "/"))
	}
	return s
}

// SameRepo reports whether two references name the same repository.
func SameRepo(a, b string) bool {
	return a != "" && b != "" && NormalizeRepo(a) == NormalizeRepo(b)
}

// Bundle picks the bundle: the explicit flag, then the target's recorded
// bundle, then the per-OS default, then the global default.
func Bundle(flag, target string, byOS map[string]string, osName, fallback string) string {
	for _, b := range []string{flag, target, byOS[osName], fallback} {
		if strings.TrimSpace(b) != "" {
			return strings.TrimSpace(b)
		}
	}
	return ""
}

// Placeholders accepted in a bootstrap template.
var Placeholders = []string{"arch", "bundle", "distro", "name", "os", "repo"}

var placeholderRE = regexp.MustCompile(`\{([a-z_]+)\}`)

// placeholders returns [start, end, keyStart, keyEnd] of each placeholder;
// "${var}" shell expansions are not placeholders.
func placeholders(tmpl string) [][]int {
	var out [][]int
	for _, m := range placeholderRE.FindAllStringSubmatchIndex(tmpl, -1) {
		if m[0] > 0 && tmpl[m[0]-1] == '$' {
			continue
		}
		out = append(out, m)
	}
	return out
}

// TemplateKeys returns the placeholders tmpl uses.
func TemplateKeys(tmpl string) []string {
	seen := map[string]bool{}
	for _, m := range placeholders(tmpl) {
		seen[tmpl[m[2]:m[3]]] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Render replaces placeholders in tmpl with shell-quoted values. Unknown
// placeholders and used-but-empty values are errors, so a typo never runs.
func Render(tmpl string, values map[string]string) (string, error) {
	known := map[string]bool{}
	for _, k := range Placeholders {
		known[k] = true
	}
	var problems []string
	for _, k := range TemplateKeys(tmpl) {
		switch {
		case !known[k]:
			problems = append(problems, fmt.Sprintf("unknown placeholder {%s} (use %s)", k, "{"+strings.Join(Placeholders, "}, {")+"}"))
		case strings.TrimSpace(values[k]) == "":
			problems = append(problems, fmt.Sprintf("{%s} is used but has no value", k))
		}
	}
	if len(problems) > 0 {
		return "", errors.New(strings.Join(problems, "; "))
	}
	var b strings.Builder
	last := 0
	for _, m := range placeholders(tmpl) {
		b.WriteString(tmpl[last:m[0]])
		b.WriteString(connection.ShellQuote(values[tmpl[m[2]:m[3]]]))
		last = m[1]
	}
	b.WriteString(tmpl[last:])
	return b.String(), nil
}

// DefaultBootstrap installs chezmoi when missing (into ~/.local/bin) and
// initializes and applies repo, answering prompts with their defaults.
const DefaultBootstrap = `set -e
if command -v chezmoi >/dev/null 2>&1; then
  cz=chezmoi
else
  if command -v curl >/dev/null 2>&1; then installer=$(curl -fsLS https://get.chezmoi.io); else installer=$(wget -qO- https://get.chezmoi.io); fi
  sh -c "$installer" -- -b "$HOME/.local/bin"
  cz="$HOME/.local/bin/chezmoi"
fi
"$cz" init --apply --promptDefaults {repo}`

// UpdateScript pulls and applies the existing source, answering new prompts
// with their defaults.
const UpdateScript = `set -e
chezmoi update --init --promptDefaults`

// BootstrapScript is the full script run on the machine: the PATH preamble
// plus the rendered template (or DefaultBootstrap).
func BootstrapScript(tmpl string, values map[string]string) (string, error) {
	if strings.TrimSpace(tmpl) == "" {
		tmpl = DefaultBootstrap
	}
	body, err := Render(tmpl, values)
	if err != nil {
		return "", err
	}
	return RemotePath + "\n" + body + "\n", nil
}
