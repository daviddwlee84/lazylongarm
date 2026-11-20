package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/daviddwlee84/lazylongarm/internal/longarm"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/skill"
	"github.com/spf13/cobra"
)

// skillSyncCommand installs agent skills with a shared-directory layout:
// one canonical directory in ~/.agents/skills (read natively by Codex,
// Copilot CLI and Gemini CLI) and relative symlinks for agents that need
// their own directory (Claude Code). Longarm keeps refreshing its own copy
// with each release; the lazylongarm skill is a stub that defers to
// `lazylongarm --skill`, so neither goes stale.
func skillSyncCommand(o *options) *cobra.Command {
	var ao applyOptions
	var noClaude bool
	c := &cobra.Command{
		Use:   "sync",
		Short: "Install the longarm and lazylongarm agent skills (~/.agents/skills + Claude symlinks)",
		Long: `Installs Longarm's bundled skill with ` + "`longarm skill install --agent codex`" + ` into
~/.agents/skills/longarm (a version-matched copy that Longarm refreshes with
each release), links ~/.claude/skills/longarm to it, and installs a stub
lazylongarm skill that tells agents to run ` + "`lazylongarm --skill`" + `.
Rerun any time; it skips what is already in place.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			client := &longarm.Client{}
			if err := o.load(); err == nil {
				if h, ok := o.cfg.Hub(""); ok {
					client.ConfigDir = h.ConfigDir
				}
			}
			p, err := buildSkillSync(home, client, !noClaude)
			if err != nil {
				return err
			}
			if ao.dryRun {
				return o.printPlan(cmd, p)
			}
			if err := o.review(cmd, p, ao); err != nil {
				return err
			}
			results, err := o.apply(cmd, p, plan.NewEnv(), ao)
			if o.json {
				_ = writeJSON(cmd.OutOrStdout(), map[string]any{"plan": p.Digest(), "results": results})
			}
			return err
		},
	}
	c.Flags().BoolVar(&noClaude, "no-claude", false, "do not create the ~/.claude/skills symlinks")
	ao.bind(c)
	return c
}

func buildSkillSync(home string, client *longarm.Client, claude bool) (*plan.Plan, error) {
	agentsDir := filepath.Join(home, ".agents", "skills")
	claudeDir := filepath.Join(home, ".claude", "skills")
	stub, err := skill.Stub()
	if err != nil {
		return nil, err
	}
	p := &plan.Plan{Kind: "skill-sync", Key: "local", Title: "Install agent skills for longarm and lazylongarm",
		Summary: [][2]string{
			{"canonical dir", agentsDir + " (Codex, Copilot CLI and Gemini CLI read it directly)"},
			{"longarm", "version-matched copy from the installed binary; Longarm refreshes it on each release"},
			{"lazylongarm", "stub that defers to `lazylongarm --skill` (never stale)"},
		}}
	if claude {
		p.Summary = append(p.Summary, [2]string{"Claude Code", claudeDir + "/{longarm,lazylongarm} → relative symlinks"})
	}
	codexState := func(ctx context.Context) (longarm.SkillAgent, error) {
		agents, err := client.SkillStatus(ctx)
		if err != nil {
			return longarm.SkillAgent{}, err
		}
		for _, a := range agents {
			if a.Agent == "codex" {
				return a, nil
			}
		}
		return longarm.SkillAgent{}, errors.New("this longarm has no codex skill target (~/.agents/skills)")
	}
	p.Steps = append(p.Steps, plan.Step{
		ID: "longarm-skill", Title: "Longarm skill → " + filepath.Join(agentsDir, "longarm"), Idempotent: true,
		Actions: []plan.Action{{Where: "local", Display: "longarm skill install --agent codex   (or `update` when outdated)"}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			a, err := codexState(ctx)
			if err != nil {
				return false, err
			}
			switch a.State {
			case "current":
				env.Logf("  longarm skill %s is current", a.InstalledVersion)
				return true, nil
			case "modified", "foreign":
				return false, plan.WithFix(fmt.Errorf("%s is %s: Longarm will not touch it", a.Dir, a.State), "inspect or remove "+a.Dir)
			}
			return false, nil
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			a, err := codexState(ctx)
			if err != nil {
				return err
			}
			verb := "install"
			if a.State == "outdated" {
				verb = "update"
			}
			out, err := client.Skill(ctx, verb, "codex")
			for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if strings.TrimSpace(l) != "" {
					env.Logf("  │ %s", l)
				}
			}
			return err
		},
	})
	if claude {
		p.Steps = append(p.Steps, linkStep(claudeDir, "longarm", client))
	}
	stubDir := filepath.Join(agentsDir, "lazylongarm")
	stubPath := filepath.Join(stubDir, "SKILL.md")
	p.Steps = append(p.Steps, plan.Step{
		ID: "lazylongarm-skill", Title: "lazylongarm stub skill → " + stubPath, Idempotent: true,
		Actions: []plan.Action{{Where: "local", Display: "write " + stubPath + " (frontmatter + `run lazylongarm --skill`)"}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			b, err := os.ReadFile(stubPath)
			return err == nil && string(b) == stub, nil
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			if fi, err := os.Lstat(stubDir); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%s is a symlink; not overwriting it", stubDir)
			}
			if b, err := os.ReadFile(stubPath); err == nil && !strings.Contains(string(b), "lazylongarm skill sync") {
				return plan.WithFix(fmt.Errorf("%s exists and was not written by lazylongarm", stubPath), "move it away, then rerun")
			}
			if err := os.MkdirAll(stubDir, 0o755); err != nil {
				return err
			}
			return os.WriteFile(stubPath, []byte(stub), 0o644)
		},
	})
	if claude {
		p.Steps = append(p.Steps, linkStep(claudeDir, "lazylongarm", nil))
	}
	return p, nil
}

// linkStep makes claudeDir/name a relative symlink to ../../.agents/skills/name.
// A copy Longarm installed for Claude is removed through Longarm first;
// anything else at that path is left alone.
func linkStep(claudeDir, name string, client *longarm.Client) plan.Step {
	link := filepath.Join(claudeDir, name)
	target := filepath.Join("..", "..", ".agents", "skills", name)
	return plan.Step{
		ID: "link-" + name, Title: "Claude Code: " + link + " → " + target, Idempotent: true,
		Actions: []plan.Action{{Where: "local", Display: "ln -s " + target + " " + link}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			got, err := os.Readlink(link)
			return err == nil && got == target, nil
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			if fi, err := os.Lstat(link); err == nil {
				switch {
				case fi.Mode()&os.ModeSymlink != 0:
					if err := os.Remove(link); err != nil {
						return err
					}
				case client != nil && name == "longarm":
					agents, err := client.SkillStatus(ctx)
					if err != nil {
						return err
					}
					for _, a := range agents {
						if a.Agent == "claude" && (a.State == "current" || a.State == "outdated") {
							env.Logf("  removing the copy Longarm installed for Claude Code")
							if _, err := client.Skill(ctx, "remove", "claude"); err != nil {
								return err
							}
						}
					}
					if _, err := os.Lstat(link); err == nil {
						return plan.WithFix(fmt.Errorf("%s exists and is not Longarm's copy", link), "inspect it; move it away to use the shared skill")
					}
				default:
					return plan.WithFix(fmt.Errorf("%s already exists", link), "move it away to link the shared skill")
				}
			}
			if err := os.MkdirAll(claudeDir, 0o755); err != nil {
				return err
			}
			return os.Symlink(target, link)
		},
	}
}
