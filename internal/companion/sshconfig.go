package companion

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// LongarmHostPrefix starts the OpenSSH Host aliases `longarm ssh-config`
// writes (Host longarm-<worker>, ProxyCommand longarm proxy <worker>).
const LongarmHostPrefix = "longarm-"

// Marker lines around the Include that lazylongarm puts at the top of
// ~/.ssh/config; RemoveInclude takes out exactly what InjectInclude added.
const (
	IncludeBegin = "# >>> lazylongarm (longarm ssh aliases) >>>"
	IncludeEnd   = "# <<< lazylongarm <<<"
)

// LongarmAlias is the ssh host that reaches worker through Longarm.
func LongarmAlias(worker string) string { return LongarmHostPrefix + worker }

var workerNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidWorkerName rejects names that could not be a Longarm worker (and
// would be unsafe in an ssh config line).
func ValidWorkerName(name string) error {
	if !workerNameRE.MatchString(name) {
		return fmt.Errorf("%q is not a Longarm worker name", name)
	}
	return nil
}

// DefaultAliasFile is where lazylongarm keeps the generated aliases.
func DefaultAliasFile(home string) string {
	return filepath.Join(home, ".ssh", "lazylongarm", "longarm.conf")
}

// UserSSHConfig is the user's OpenSSH client config.
func UserSSHConfig(home string) string { return filepath.Join(home, ".ssh", "config") }

// IncludeLine is the Include directive for file, written with ~ when file
// is under home (so a dotfiles repository can carry it unchanged). A file in
// lazylongarm's own directory is included by a glob of that directory:
// OpenSSH skips a glob that matches nothing, so the same line can sit in
// dotfiles on every machine, and later files need no new line.
func IncludeLine(file, home string) string {
	p := file
	if home != "" && filepath.Dir(file) == filepath.Dir(DefaultAliasFile(home)) && strings.HasSuffix(file, ".conf") {
		p = filepath.Join(filepath.Dir(file), "*.conf")
	}
	if home != "" {
		if rel, err := filepath.Rel(home, p); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			p = "~/" + filepath.ToSlash(rel)
		}
	}
	if strings.ContainsAny(p, " \t\"") {
		p = `"` + strings.ReplaceAll(p, `"`, `\"`) + `"`
	}
	return "Include " + p
}

// newline is the line ending content uses ("\r\n" when it has any).
func newline(content []byte) string {
	if bytes.Contains(content, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

// HasInclude reports whether content (the user's ssh config) includes
// file through a top-level Include line: lazylongarm's block, a line the
// user wrote, or a glob covering it (Include ~/.ssh/conf.d/*.conf). Include
// lines inside a Host or Match block only apply there and do not count.
func HasInclude(content []byte, file, home string) bool {
	for l := range strings.SplitSeq(string(content), "\n") {
		f := strings.Fields(strings.TrimSpace(l))
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		key := strings.ToLower(f[0])
		if key == "host" || key == "match" {
			return false
		}
		if key != "include" {
			continue
		}
		for _, pat := range f[1:] {
			pat = strings.Trim(pat, `"`)
			if rest, ok := strings.CutPrefix(pat, "~/"); ok {
				pat = filepath.Join(home, rest)
			} else if !filepath.IsAbs(pat) {
				pat = filepath.Join(home, ".ssh", pat) // relative to ~/.ssh, as OpenSSH reads it
			}
			if ok, _ := filepath.Match(filepath.Clean(pat), filepath.Clean(file)); ok {
				return true
			}
		}
	}
	return false
}

// InjectInclude puts the marked Include block at the very top of content:
// ssh reads Include lines before any Host block for every host. A file that
// already has the block gets its line replaced when it differs. The result
// is content unchanged plus the block, so RemoveInclude restores it byte
// for byte.
func InjectInclude(content []byte, line string) ([]byte, bool) {
	nl := newline(content)
	if lo, hi, ok := blockSpan(content); ok {
		block := IncludeBegin + nl + line + nl + IncludeEnd + nl
		if string(content[lo:hi]) == block {
			return content, false
		}
		out := append([]byte{}, content[:lo]...)
		out = append(out, block...)
		return append(out, content[hi:]...), true
	}
	block := IncludeBegin + nl + line + nl + IncludeEnd + nl
	if len(content) > 0 {
		block += nl // a blank line before the user's own first line
	}
	return append([]byte(block), content...), true
}

// RemoveInclude takes out the lazylongarm block and the blank line
// InjectInclude added after it.
func RemoveInclude(content []byte) ([]byte, bool) {
	lo, hi, ok := blockSpan(content)
	if !ok {
		return content, false
	}
	rest := content[hi:]
	if bytes.HasPrefix(rest, []byte("\r\n")) {
		rest = rest[2:]
	} else if bytes.HasPrefix(rest, []byte("\n")) {
		rest = rest[1:]
	}
	out := append([]byte{}, content[:lo]...)
	return append(out, rest...), true
}

// blockSpan finds the marked block, from the start of its first line to the
// end of its last line (including the line ending).
func blockSpan(content []byte) (int, int, bool) {
	lo := -1
	pos := 0
	for pos <= len(content) {
		end := bytes.IndexByte(content[pos:], '\n')
		next := len(content)
		if end >= 0 {
			next = pos + end + 1
		}
		line := strings.TrimRight(string(content[pos:next]), "\r\n")
		switch {
		case lo < 0 && line == IncludeBegin:
			lo = pos
		case lo >= 0 && line == IncludeEnd:
			return lo, next, true
		}
		if end < 0 {
			break
		}
		pos = next
	}
	return 0, 0, false
}

// MergeAliases returns the alias file with generated's Host blocks added,
// replacing blocks for the same hosts and keeping the others (aliases for
// workers set up earlier stay usable).
func MergeAliases(existing, generated []byte) []byte {
	old, gen := hostBlocks(existing), hostBlocks(generated)
	maps.Copy(old, gen)
	hosts := make([]string, 0, len(old))
	for h := range old {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	var b strings.Builder
	b.WriteString("# Generated by lazylongarm from `longarm ssh-config`; rewritten by\n")
	b.WriteString("# `lazylongarm companion ssh-config`. Edits here are lost.\n\n")
	for _, h := range hosts {
		b.WriteString(strings.TrimRight(old[h], "\n"))
		b.WriteString("\n\n")
	}
	return []byte(b.String())
}

// AliasHosts lists the Host names in an alias file.
func AliasHosts(content []byte) []string {
	var out []string
	for h := range hostBlocks(content) {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// hostBlocks splits ssh config text into Host blocks keyed by the first
// host name; text before the first Host line is dropped.
func hostBlocks(content []byte) map[string]string {
	out := map[string]string{}
	var cur string
	var b strings.Builder
	flush := func() {
		if cur != "" {
			out[cur] = b.String()
		}
		b.Reset()
	}
	for l := range strings.SplitSeq(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 && strings.EqualFold(f[0], "host") {
			flush()
			cur = f[1]
		}
		if cur != "" {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	flush()
	return out
}

// ProxyCommand returns the ProxyCommand ssh resolves for host (`ssh -G`),
// "" when none.
func (t Tools) ProxyCommand(ctx context.Context, host string) (string, error) {
	out, err := t.Run(ctx, "ssh", "-G", "--", host)
	if err != nil {
		return "", err
	}
	for l := range strings.SplitSeq(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), " "); ok && strings.EqualFold(k, "proxycommand") {
			if v == "none" {
				return "", nil
			}
			return v, nil
		}
	}
	return "", nil
}

// ResolvesThroughLongarm reports whether a ProxyCommand reaches worker
// through `longarm proxy`.
func ResolvesThroughLongarm(proxy, worker string) bool {
	f := strings.Fields(proxy)
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "proxy" && f[i+1] == worker {
			return strings.Contains(proxy, "longarm")
		}
	}
	return false
}

// ChezmoiManaged reports whether chezmoi manages path (then editing the
// target would be undone, or show as drift, at the next `chezmoi apply`).
func (t Tools) ChezmoiManaged(ctx context.Context, path string) bool {
	if !t.Available("chezmoi") {
		return false
	}
	_, err := t.Run(ctx, "chezmoi", "source-path", "--", path)
	return err == nil
}

// WriteFileAtomic writes data through a temporary file in the same
// directory and renames it over path (following a symlink to its target),
// keeping an existing file's mode. dirMode is used for a missing directory.
func WriteFileAtomic(path string, data []byte, mode, dirMode os.FileMode) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
