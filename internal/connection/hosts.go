package connection

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SuggestHosts lists concrete Host aliases from ~/.ssh/config and the files
// it Includes, for pickers only. OpenSSH remains the authority on what an
// alias means (Match blocks, ProxyJump, tokens); this never resolves options.
func SuggestHosts() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	visited := map[string]bool{}
	var scan func(path string, depth int)
	scan = func(path string, depth int) {
		if depth > 4 || visited[path] {
			return
		}
		visited[path] = true
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, rest := splitKeyword(line)
			switch strings.ToLower(key) {
			case "host":
				for _, h := range strings.Fields(rest) {
					if strings.ContainsAny(h, "*?!") || seen[h] || ValidateHost(h) != nil {
						continue
					}
					seen[h] = true
					out = append(out, h)
				}
			case "include":
				for _, pattern := range strings.Fields(rest) {
					if strings.HasPrefix(pattern, "~/") {
						pattern = filepath.Join(home, pattern[2:])
					} else if !filepath.IsAbs(pattern) {
						pattern = filepath.Join(home, ".ssh", pattern)
					}
					matches, _ := filepath.Glob(pattern)
					sort.Strings(matches)
					for _, m := range matches {
						scan(m, depth+1)
					}
				}
			}
		}
	}
	scan(filepath.Join(home, ".ssh", "config"), 0)
	return out
}

func splitKeyword(line string) (string, string) {
	i := strings.IndexAny(line, " \t=")
	if i < 0 {
		return line, ""
	}
	rest := strings.TrimLeft(line[i:], " \t=")
	return line[:i], rest
}

// ResolveHostname returns the HostName OpenSSH would connect to for alias
// (from `ssh -G`), for use as a default display/address only.
func ResolveHostname(ctx context.Context, alias string) string {
	if ValidateHost(alias) != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := commandContext(ctx, "ssh", "-G", "--", alias).Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "hostname "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
