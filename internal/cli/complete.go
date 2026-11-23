package cli

import (
	"slices"
	"strings"

	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/skill"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Shell completion. Completion functions only read local files (the config
// and ~/.ssh/config): no SSH, no network, no writes, no prompts, nothing on
// stderr. Any error yields no candidates rather than file names.

type completeFunc = func(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective)

// completionConfig loads the config for a completion without the warnings
// o.load prints and without caching a partial result.
func (o *options) completionConfig() *config.Config {
	if o.cfg != nil {
		return o.cfg
	}
	path, explicit, err := config.ResolvePath(o.configPath)
	if err != nil {
		return nil
	}
	cfg, err := config.Load(path, explicit)
	if err != nil {
		return nil
	}
	return cfg
}

// candidates turns value→description pairs into completions, dropping
// values already given as arguments and empty values.
func candidates(args []string, pairs [][2]string) []cobra.Completion {
	var out []cobra.Completion
	seen := map[string]bool{}
	for _, a := range args {
		seen[a] = true
	}
	for _, p := range pairs {
		if p[0] == "" || seen[p[0]] {
			continue
		}
		seen[p[0]] = true
		out = append(out, cobra.CompletionWithDesc(p[0], p[1]))
	}
	return out
}

// completeSSHHost completes one SSH Host alias from ~/.ssh/config.
func completeSSHHost(_ *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var pairs [][2]string
	for _, h := range connection.SuggestHosts() {
		pairs = append(pairs, [2]string{h, ""})
	}
	return candidates(nil, pairs), cobra.ShellCompDirectiveNoFileComp
}

// completeHubID completes the --hub flag from the configured hubs.
func (o *options) completeHubID(_ *cobra.Command, _ []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	cfg := o.completionConfig()
	if cfg == nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var pairs [][2]string
	for _, h := range cfg.Hubs {
		desc := h.Name
		if desc == h.ID {
			desc = ""
		}
		if h.ID == cfg.DefaultHub {
			desc = strings.TrimSpace(desc + " (default)")
		}
		pairs = append(pairs, [2]string{h.ID, desc})
	}
	return candidates(nil, pairs), cobra.ShellCompDirectiveNoFileComp
}

// completeMachines completes fleet machines (remote targets by name), any
// number of them. With workers it offers their Longarm worker names instead.
func (o *options) completeMachines(workers bool) completeFunc {
	return func(_ *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
		cfg := o.completionConfig()
		if cfg == nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var pairs [][2]string
		for _, t := range cfg.Targets {
			if t.SSHHost == "" {
				continue
			}
			name := t.Name
			if name == "" {
				name = t.ID
			}
			if workers {
				name = targetWorker(t)
			}
			desc := ""
			if t.SSHHost != name {
				desc = "ssh " + t.SSHHost
			}
			pairs = append(pairs, [2]string{name, desc})
		}
		return candidates(args, pairs), cobra.ShellCompDirectiveNoFileComp
	}
}

// completeSkillTopic completes `skill print [topic]`.
func completeSkillTopic(_ *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var pairs [][2]string
	for _, t := range skill.Topics() {
		pairs = append(pairs, [2]string{t, ""})
	}
	return candidates(nil, pairs), cobra.ShellCompDirectiveNoFileComp
}

// fileFlags take paths: they keep the shell's file completion.
var fileFlags = []string{"config", "out", "file", "auth-key-file"}

// wireCompletion runs once on the finished tree: --hub completes hub ids,
// --color its values, and every command without its own argument
// completion stops offering file names (no lazylongarm argument is a path).
func (o *options) wireCompletion(c *cobra.Command) {
	if c.ValidArgsFunction == nil && len(c.ValidArgs) == 0 {
		c.ValidArgsFunction = cobra.NoFileCompletions
	}
	if c.LocalNonPersistentFlags().Lookup("hub") != nil {
		_ = c.RegisterFlagCompletionFunc("hub", o.completeHubID)
	}
	if c.LocalNonPersistentFlags().Lookup("host") != nil {
		_ = c.RegisterFlagCompletionFunc("host", func(cmd *cobra.Command, _ []string, s string) ([]cobra.Completion, cobra.ShellCompDirective) {
			return completeSSHHost(cmd, nil, s)
		})
	}
	if !c.HasParent() {
		_ = c.RegisterFlagCompletionFunc("color", cobra.FixedCompletions([]cobra.Completion{"auto", "always", "never"}, cobra.ShellCompDirectiveNoFileComp))
	}
	c.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
		if f.Name == "hub" || f.Name == "host" || slices.Contains(fileFlags, f.Name) || f.Value.Type() == "bool" {
			return
		}
		if _, ok := c.GetFlagCompletionFunc(f.Name); ok {
			return
		}
		_ = c.RegisterFlagCompletionFunc(f.Name, cobra.NoFileCompletions)
	})
	for _, sub := range c.Commands() {
		o.wireCompletion(sub)
	}
}
