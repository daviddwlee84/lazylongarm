package flows

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/daviddwlee84/lazylongarm/internal/companion"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
)

// How the alias file reaches ssh (`--include`).
const (
	IncludeAuto = "auto" // edit ~/.ssh/config unless chezmoi manages it
	IncludeYes  = "yes"  // edit ~/.ssh/config
	IncludeNo   = "no"   // never edit it; print the line to add
)

// SSHConfigRequest asks for OpenSSH aliases (Host longarm-<worker>) that
// reach Workers through `longarm proxy`.
type SSHConfigRequest struct {
	Workers    []string
	File       string // the alias file lazylongarm owns
	UserConfig string // ~/.ssh/config
	Home       string // for the ~ form of the Include line
	Include    string // IncludeAuto | IncludeYes | IncludeNo
}

// SSHConfigState is read before planning.
type SSHConfigState struct {
	Resolves       map[string]bool // worker → ssh -G already resolves its alias through longarm
	IncludePresent bool            // ~/.ssh/config already includes File (our block or the user's own line)
	ChezmoiManaged bool            // chezmoi manages ~/.ssh/config
}

// SSHConfigDeps runs the tools the steps need.
type SSHConfigDeps struct {
	Tools   companion.Tools                                           // ssh -G
	Longarm func(ctx context.Context, args ...string) ([]byte, error) // longarm with the hub's config dir
}

// ReadSSHConfigState checks what ssh resolves and what ~/.ssh/config has.
func ReadSSHConfigState(ctx context.Context, req SSHConfigRequest, tools companion.Tools) (SSHConfigState, error) {
	st := SSHConfigState{Resolves: map[string]bool{}}
	for _, w := range req.Workers {
		proxy, err := tools.ProxyCommand(ctx, companion.LongarmAlias(w))
		if err != nil {
			return st, fmt.Errorf("ssh -G %s: %w", companion.LongarmAlias(w), err)
		}
		st.Resolves[w] = companion.ResolvesThroughLongarm(proxy, w)
	}
	data, err := os.ReadFile(req.UserConfig)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return st, err
	}
	st.IncludePresent = companion.HasInclude(data, req.File, req.Home)
	st.ChezmoiManaged = tools.ChezmoiManaged(ctx, req.UserConfig)
	return st, nil
}

// BuildCompanionSSHConfig plans the aliases for the workers ssh does not
// resolve through longarm yet.
func BuildCompanionSSHConfig(req SSHConfigRequest, st SSHConfigState, deps SSHConfigDeps) (*plan.Plan, error) {
	if len(req.Workers) == 0 {
		return nil, fmt.Errorf("no workers selected")
	}
	p := &plan.Plan{Kind: "companion-ssh-config", Key: "local", Title: "Longarm ssh aliases (Host longarm-<worker>)"}
	if err := addSSHConfigSteps(p, req, st, deps); err != nil {
		return nil, err
	}
	return p, nil
}

// addSSHConfigSteps adds the alias steps (shared with `companion remotes
// --transport longarm`): write the alias file, include it from
// ~/.ssh/config, and check that ssh resolves each alias through longarm.
func addSSHConfigSteps(p *plan.Plan, req SSHConfigRequest, st SSHConfigState, deps SSHConfigDeps) error {
	switch req.Include {
	case "":
		req.Include = IncludeAuto
	case IncludeAuto, IncludeYes, IncludeNo:
	default:
		return fmt.Errorf("--include must be auto, yes or no, not %q", req.Include)
	}
	var missing, aliases []string
	for _, w := range req.Workers {
		if err := companion.ValidWorkerName(w); err != nil {
			return err
		}
		aliases = append(aliases, companion.LongarmAlias(w))
		if !st.Resolves[w] {
			missing = append(missing, w)
		}
	}
	line := companion.IncludeLine(req.File, req.Home)
	p.Summary = append(p.Summary, [2]string{"aliases", strings.Join(aliases, ", ")})
	if len(missing) == 0 {
		p.Summary = append(p.Summary, [2]string{"ssh", "already reaches them through longarm"})
		return nil
	}
	p.Summary = append(p.Summary, [2]string{"alias file", req.File})
	missingAliases := make([]string, len(missing))
	for i, w := range missing {
		missingAliases[i] = companion.LongarmAlias(w)
	}

	p.Steps = append(p.Steps, plan.Step{
		ID:         "ssh-aliases",
		Title:      "Write Longarm ssh aliases to " + req.File,
		Idempotent: true,
		Actions: []plan.Action{{Where: "local", Display: "longarm ssh-config " + strings.Join(missing, " ") + "  → merged into " + req.File +
			"\n(also records the workers' sshd host keys, fetched over mTLS, in Longarm's known_hosts)"}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			out, err := deps.Longarm(ctx, append([]string{"ssh-config"}, missing...)...)
			if err != nil {
				return err
			}
			got := map[string]bool{}
			for _, h := range companion.AliasHosts(out) {
				got[h] = true
			}
			for _, a := range missingAliases {
				if !got[a] {
					return fmt.Errorf("longarm ssh-config printed no entry for %s", a)
				}
			}
			old, err := os.ReadFile(req.File)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return companion.WriteFileAtomic(req.File, companion.MergeAliases(old, out), 0o600, 0o700)
		},
	})

	inject := !st.IncludePresent
	switch {
	case st.IncludePresent:
		p.Summary = append(p.Summary, [2]string{"~/.ssh/config", "already includes the alias file"})
	case req.Include == IncludeNo:
		inject = false
		p.Warnings = append(p.Warnings, fmt.Sprintf("--include no: add this line at the top of %s yourself (before any Host block):\n    %s", req.UserConfig, line))
	case req.Include == IncludeAuto && st.ChezmoiManaged:
		inject = false
		p.Warnings = append(p.Warnings, fmt.Sprintf("chezmoi manages %s, so lazylongarm leaves it alone (chezmoi apply would undo the edit). Add this line at the top of its source (`chezmoi edit %s`), then `chezmoi apply`:\n    %s\n  or pass --include yes to edit the file anyway", req.UserConfig, req.UserConfig, line))
	}
	if inject {
		p.Summary = append(p.Summary, [2]string{"~/.ssh/config", "gets a marked Include block at the top (undo: lazylongarm companion ssh-config --remove)"})
		p.Steps = append(p.Steps, plan.Step{
			ID:         "ssh-include",
			Title:      "Include the alias file from " + req.UserConfig,
			Idempotent: true,
			Actions: []plan.Action{{Where: "local", Display: "add at the top of " + req.UserConfig + " (backup: " + backupPath(req.UserConfig) + "):\n" +
				companion.IncludeBegin + "\n" + line + "\n" + companion.IncludeEnd}},
			Check: func(ctx context.Context, env *plan.Env) (bool, error) {
				data, err := os.ReadFile(req.UserConfig)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					return false, err
				}
				return companion.HasInclude(data, req.File, req.Home), nil
			},
			Apply: func(ctx context.Context, env *plan.Env) error {
				return editUserConfig(req.UserConfig, func(b []byte) ([]byte, bool) { return companion.InjectInclude(b, line) })
			},
		})
	}

	p.Steps = append(p.Steps, plan.Step{
		ID:       "ssh-resolve",
		Title:    "Check that ssh reaches " + strings.Join(missingAliases, ", ") + " through longarm",
		ReadOnly: true,
		Actions:  []plan.Action{{Where: "local", Display: "ssh -G <alias>: ProxyCommand longarm … proxy <worker>"}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			for _, w := range missing {
				proxy, err := deps.Tools.ProxyCommand(ctx, companion.LongarmAlias(w))
				if err != nil {
					return err
				}
				if !companion.ResolvesThroughLongarm(proxy, w) {
					return plan.WithFix(fmt.Errorf("ssh does not resolve %s through longarm (ProxyCommand %q)", companion.LongarmAlias(w), proxy),
						fmt.Sprintf("add `%s` at the top of %s (before any Host block), or rerun with --include yes", line, req.UserConfig))
				}
			}
			return nil
		},
	})
	return nil
}

// BuildCompanionSSHConfigRemove plans undoing BuildCompanionSSHConfig: the
// marked block in ~/.ssh/config and the alias file. An Include line the
// user wrote themselves stays.
func BuildCompanionSSHConfigRemove(req SSHConfigRequest) *plan.Plan {
	p := &plan.Plan{Kind: "companion-ssh-config-remove", Key: "local", Title: "Remove lazylongarm's Longarm ssh aliases"}
	p.Summary = append(p.Summary, [2]string{"alias file", req.File}, [2]string{"~/.ssh/config", "the marked lazylongarm block only; the rest is left as it is"})
	p.Warnings = append(p.Warnings, "hc/herdr machines registered as longarm-<worker> stop connecting; remove them there (hc machine rm, herdr machine remove)")
	hasBlock := func() (bool, error) {
		data, err := os.ReadFile(req.UserConfig)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		_, found := companion.RemoveInclude(data)
		return found, err
	}
	p.Steps = append(p.Steps,
		plan.Step{
			ID:         "ssh-include-remove",
			Title:      "Remove the lazylongarm Include block from " + req.UserConfig,
			Idempotent: true,
			Actions:    []plan.Action{{Where: "local", Display: "delete the lines between the lazylongarm markers (backup: " + backupPath(req.UserConfig) + ")"}},
			Check: func(ctx context.Context, env *plan.Env) (bool, error) {
				found, err := hasBlock()
				return !found, err
			},
			Apply: func(ctx context.Context, env *plan.Env) error {
				return editUserConfig(req.UserConfig, companion.RemoveInclude)
			},
		},
		plan.Step{
			ID:         "ssh-aliases-remove",
			Title:      "Delete " + req.File,
			Idempotent: true,
			Actions:    []plan.Action{{Where: "local", Display: "rm " + req.File}},
			Check: func(ctx context.Context, env *plan.Env) (bool, error) {
				_, err := os.Stat(req.File)
				return errors.Is(err, os.ErrNotExist), nil
			},
			Apply: func(ctx context.Context, env *plan.Env) error {
				if err := os.Remove(req.File); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				_ = os.Remove(filepath.Dir(req.File)) // only when empty
				return nil
			},
		})
	return p
}

func backupPath(path string) string { return path + ".lazylongarm.bak" }

// editUserConfig rewrites ~/.ssh/config through edit, keeping a backup of
// the previous content. A missing file is treated as empty.
func editUserConfig(path string, edit func([]byte) ([]byte, bool)) error {
	data, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	out, changed := edit(data)
	if !changed {
		return nil
	}
	if exists {
		if err := companion.WriteFileAtomic(backupPath(path), data, 0o600, 0o700); err != nil {
			return fmt.Errorf("back up %s: %w", path, err)
		}
	}
	return companion.WriteFileAtomic(path, out, 0o600, 0o700)
}
