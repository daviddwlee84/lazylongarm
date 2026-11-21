// Package companion adapts the tools lazylongarm sets up next to Longarm on
// fleet machines: hc and herdr (which keep their own lists of remote
// machines on this computer) and chezmoi dotfiles (on the machine itself).
// lazylongarm only calls their public CLIs; it never writes their state
// files.
package companion

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Tool names.
const (
	HC    = "hc"
	Herdr = "herdr"
)

// RemoteTools lists the remote-machine tools in display order.
var RemoteTools = []string{HC, Herdr}

// Remote is one machine registered in hc or herdr.
type Remote struct {
	ID      string `json:"id,omitempty"`      // herdr profile id (hc: none, it goes by name)
	Name    string `json:"name"`              // hc name / herdr label
	Target  string `json:"target"`            // how ssh reaches it
	State   string `json:"state,omitempty"`   // hc: connected, ...
	Enabled bool   `json:"enabled"`           // herdr: enabled; hc: always true
	Version string `json:"version,omitempty"` // hc on the remote
}

// Runner runs a local program and returns its stdout (a test seam).
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Tools finds and queries hc/herdr on this machine.
type Tools struct {
	Run      Runner
	LookPath func(string) (string, error)
}

// System returns Tools backed by exec.
func System() Tools {
	return Tools{
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			out, err := exec.CommandContext(ctx, name, args...).Output()
			if ee, ok := err.(*exec.ExitError); ok {
				return out, fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
			}
			return out, err
		},
		LookPath: exec.LookPath,
	}
}

// Available reports whether tool is installed here.
func (t Tools) Available(tool string) bool {
	if t.LookPath == nil {
		return false
	}
	_, err := t.LookPath(tool)
	return err == nil
}

// List returns the machines tool knows.
func (t Tools) List(ctx context.Context, tool string) ([]Remote, error) {
	switch tool {
	case HC:
		out, err := t.Run(ctx, HC, "machine", "ls", "--json")
		if err != nil {
			return nil, err
		}
		return ParseHC(out)
	case Herdr:
		out, err := t.Run(ctx, Herdr, "machine", "list", "--json")
		if err != nil {
			return nil, err
		}
		return ParseHerdr(out)
	}
	return nil, fmt.Errorf("unknown tool %q", tool)
}

// ParseHC decodes `hc machine ls --json` ({"machines":[...]}).
func ParseHC(b []byte) ([]Remote, error) {
	var doc struct {
		Machines []struct {
			Name    string `json:"name"`
			Target  string `json:"target"`
			State   string `json:"state"`
			Version string `json:"version"`
		} `json:"machines"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("hc machine ls --json: %w", err)
	}
	out := make([]Remote, 0, len(doc.Machines))
	for _, m := range doc.Machines {
		out = append(out, Remote{Name: m.Name, Target: m.Target, State: m.State, Enabled: true, Version: m.Version})
	}
	return out, nil
}

// ParseHerdr decodes `herdr machine list --json` ([{label,target,enabled}]).
func ParseHerdr(b []byte) ([]Remote, error) {
	var doc []struct {
		ID      string `json:"id"`
		Label   string `json:"label"`
		Target  string `json:"target"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("herdr machine list --json: %w", err)
	}
	out := make([]Remote, 0, len(doc))
	for _, m := range doc {
		state := "enabled"
		if !m.Enabled {
			state = "disabled"
		}
		out = append(out, Remote{ID: m.ID, Name: m.Label, Target: m.Target, State: state, Enabled: m.Enabled})
	}
	return out, nil
}

// Find returns the entry that refers to the machine reached as sshHost or
// named name. A match on the ssh target wins over a match on the name.
func Find(list []Remote, sshHost, name string) (Remote, bool) {
	for _, r := range list {
		if sshHost != "" && r.Target == sshHost {
			return r, true
		}
	}
	for _, r := range list {
		if (name != "" && r.Name == name) || (sshHost != "" && r.Name == sshHost) {
			return r, true
		}
	}
	return Remote{}, false
}

// FindLongarm returns the entry that reaches worker through Longarm: the
// alias longarm-<worker>, or hc's --via 'longarm ssh <worker> --'.
func FindLongarm(list []Remote, worker string) (Remote, bool) {
	for _, r := range list {
		if r.Target == LongarmAlias(worker) || r.viaWorker() == worker {
			return r, true
		}
	}
	return Remote{}, false
}

// viaWorker is the worker of a `longarm ssh <worker> ...` command target.
func (r Remote) viaWorker() string {
	f := strings.Fields(r.Target)
	if len(f) >= 3 && filepathBase(f[0]) == "longarm" && f[1] == "ssh" {
		return f[2]
	}
	return ""
}

func filepathBase(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// ViaLongarm reports whether an entry reaches its machine through Longarm.
func (r Remote) ViaLongarm() bool {
	return strings.HasPrefix(r.Target, LongarmHostPrefix) || r.viaWorker() != ""
}

// RemoveArgv is the command that forgets r in tool (the machine and its
// terminals are left alone: hc keeps them running, herdr only drops the
// profile).
func RemoveArgv(tool string, r Remote) []string {
	switch tool {
	case HC:
		return []string{HC, "machine", "rm", r.Name}
	case Herdr:
		return []string{Herdr, "machine", "remove", r.ID}
	}
	return nil
}

// AddArgv is the command that registers sshHost under name in tool.
// hc copies itself to the machine when it is missing or too old (--install);
// herdr prepares its server there and may ask for approval in a terminal.
func AddArgv(tool, sshHost, name string) []string {
	switch tool {
	case HC:
		argv := []string{HC, "machine", "add", sshHost, "--install"}
		if name != "" {
			argv = append(argv, "--name", name)
		}
		return argv
	case Herdr:
		argv := []string{Herdr, "machine", "add", sshHost}
		if name != "" {
			argv = append(argv, "--label", name)
		}
		return argv
	}
	return nil
}
