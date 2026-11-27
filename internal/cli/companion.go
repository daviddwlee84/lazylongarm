package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/companion"
	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/flows"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/wizard"
	"github.com/spf13/cobra"
)

func companionCommand(o *options) *cobra.Command {
	c := &cobra.Command{
		Use:   "companion",
		Short: "Set up companion tools for fleet machines: hc/herdr remotes, chezmoi dotfiles, ferry",
		Long: `lazylongarm already knows the fleet's machines, so it can set up the tools you
use next to Longarm without retyping hosts:

  remotes     register the machines in hc and herdr on this computer
              (over their SSH hosts, or keyless through Longarm)
  ssh-config  OpenSSH aliases longarm-<worker> through Longarm's mTLS channel
  dotfiles    bootstrap (or update) chezmoi dotfiles on the machines
  ferry       bulk transfers through a cloud relay instead of the hub's relay
  status      show which machines have what

Personal values (your dotfiles repository, bundles, a bootstrap command) live in
the [companions.dotfiles] table of lazylongarm's config; ssh-config settings in
[companions.ssh]; ferry settings and the jobs it wrote in [companions.ferry].`,
		Args: cobra.ArbitraryArgs, RunE: groupHelp,
	}
	c.AddCommand(companionStatusCommand(o), companionRemotesCommand(o), companionSSHConfigCommand(o), companionDotfilesCommand(o), companionFerryCommand(o))
	return c
}

// remoteTargets are the configured machines reached over SSH.
func (o *options) remoteTargets() []config.Target {
	var out []config.Target
	for _, t := range o.cfg.Targets {
		if t.SSHHost != "" {
			out = append(out, t)
		}
	}
	return out
}

// pickTargets resolves host arguments (target id/name or SSH host), --all,
// or a multi-select in a terminal. Unknown hosts become ad-hoc targets.
func (o *options) pickTargets(cmd *cobra.Command, args []string, all bool, title, desc string, preselect func(config.Target) bool) ([]config.Target, error) {
	return o.pickTargetsAs(cmd, args, all, false, title, desc, preselect)
}

// targetWorker is the Longarm worker name of a target.
func targetWorker(t config.Target) string {
	if t.WorkerName != "" {
		return t.WorkerName
	}
	return t.Name
}

// pickTargetsAs is pickTargets; with workers, unknown arguments are Longarm
// worker names (and a target's worker name matches too) instead of SSH hosts.
func (o *options) pickTargetsAs(cmd *cobra.Command, args []string, all, workers bool, title, desc string, preselect func(config.Target) bool) ([]config.Target, error) {
	known := o.remoteTargets()
	if all {
		if len(args) > 0 {
			return nil, usage("give machines or --all, not both")
		}
		if len(known) == 0 {
			return nil, usage("no fleet machines yet: add one with `lazylongarm worker add`, or name SSH hosts")
		}
		return known, nil
	}
	if len(args) > 0 {
		var out []config.Target
		for _, a := range args {
			if t, ok := o.cfg.TargetByName(a); ok && t.SSHHost != "" {
				out = append(out, *t)
			} else if t, ok := o.cfg.TargetByHost(a); ok && a != "" {
				out = append(out, *t)
			} else if t, ok := o.targetByWorker(a); ok && workers {
				out = append(out, t)
			} else if workers {
				if err := companion.ValidWorkerName(a); err != nil {
					return nil, usage("%v", err)
				}
				out = append(out, config.Target{ID: a, Name: a, WorkerName: a})
			} else {
				if err := connection.ValidateHost(a); err != nil {
					return nil, usage("%v", err)
				}
				out = append(out, config.Target{ID: a, Name: a, SSHHost: a})
			}
		}
		return out, nil
	}
	if !o.interactive(cmd) {
		return nil, usage("name the machines (or pass --all) when not in a terminal")
	}
	if len(known) == 0 {
		return nil, usage("no fleet machines yet: add one with `lazylongarm worker add`, or name SSH hosts")
	}
	choices := make([]wizard.Choice, 0, len(known))
	var selected []string
	for _, t := range known {
		label := t.Name
		if t.SSHHost != t.Name {
			label += "  (" + t.SSHHost + ")"
		}
		choices = append(choices, wizard.Choice{Value: t.ID, Label: label})
		if preselect == nil || preselect(t) {
			selected = append(selected, t.ID)
		}
	}
	ids, err := wizard.MultiChoose(cmd.Context(), wizard.MultiSpec{Title: title, Description: desc, Choices: choices, Selected: selected}, cmd.InOrStdin(), cmd.OutOrStdout())
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []config.Target
	for _, t := range known {
		if want[t.ID] {
			out = append(out, t)
		}
	}
	return out, nil
}

// targetByWorker finds the remote target whose Longarm worker is name.
func (o *options) targetByWorker(name string) (config.Target, bool) {
	for _, t := range o.remoteTargets() {
		if targetWorker(t) == name {
			return t, true
		}
	}
	return config.Target{}, false
}

// remoteLists reads hc/herdr machine lists; a tool that is not installed is
// absent from the map, and a tool whose list cannot be read is in errs.
func (o *options) remoteLists(ctx context.Context, tools []string) (map[string][]companion.Remote, map[string]error) {
	lists, errs := map[string][]companion.Remote{}, map[string]error{}
	for _, tool := range tools {
		if !o.deps.Companion.Available(tool) {
			continue
		}
		lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		list, err := o.deps.Companion.List(lctx, tool)
		cancel()
		if err != nil {
			errs[tool] = err
			continue
		}
		lists[tool] = list
	}
	return lists, errs
}

func validTools(tools []string) ([]string, error) {
	if len(tools) == 0 {
		return companion.RemoteTools, nil
	}
	for _, t := range tools {
		if t != companion.HC && t != companion.Herdr {
			return nil, usage("unknown tool %q (use hc, herdr)", t)
		}
	}
	return tools, nil
}

// ---- companion status ----

type companionRow struct {
	Name     string            `json:"name"`
	SSHHost  string            `json:"ssh_host"`
	Remotes  map[string]string `json:"remotes"` // tool → registered state | missing | not installed
	Dotfiles *dotfilesStatus   `json:"dotfiles,omitempty"`
	Ferry    *ferryStatus      `json:"ferry,omitempty"`
}

type dotfilesStatus struct {
	State  string `json:"state"` // ok | other-repo | chezmoi-only | not-set-up | error
	Origin string `json:"origin,omitempty"`
	Rev    string `json:"rev,omitempty"`
	Bundle string `json:"bundle,omitempty"`
	Error  string `json:"error,omitempty"`
}

func companionStatusCommand(o *options) *cobra.Command {
	var noRemote bool
	c := &cobra.Command{
		Use:   "status",
		Short: "Show hc/herdr registration, dotfiles and ferry state for each fleet machine",
		Long: `hc and herdr columns come from their machine lists on this computer. The
dotfiles and ferry columns run read-only checks over SSH on each machine (skip
them with --no-remote). The ferry column shows its version and, per installed
backend, whether it is signed in (never which account).`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.load(); err != nil {
				return err
			}
			lists, listErrs := o.remoteLists(cmd.Context(), companion.RemoteTools)
			targets := o.remoteTargets()
			rows := make([]companionRow, len(targets))
			for i, t := range targets {
				rows[i] = companionRow{Name: t.Name, SSHHost: t.SSHHost, Remotes: map[string]string{}}
				for _, tool := range companion.RemoteTools {
					list, ok := lists[tool]
					r, found := companion.Find(list, t.SSHHost, t.Name)
					lr, viaLongarm := companion.FindLongarm(list, targetWorker(t))
					switch {
					case listErrs[tool] != nil:
						rows[i].Remotes[tool] = "error"
					case !ok:
						rows[i].Remotes[tool] = "not installed"
					case found && r.ViaLongarm():
						rows[i].Remotes[tool] = dash(r.State) + " (longarm)"
					case found && viaLongarm:
						rows[i].Remotes[tool] = dash(r.State) + " +longarm"
					case found:
						rows[i].Remotes[tool] = dash(r.State)
					case viaLongarm:
						rows[i].Remotes[tool] = dash(lr.State) + " (longarm)"
					default:
						rows[i].Remotes[tool] = "missing"
					}
				}
			}
			if !noRemote {
				repo := o.cfg.Companions.Dotfiles.Repo
				var wg sync.WaitGroup
				for i, t := range targets {
					wg.Add(1)
					go func() {
						defer wg.Done()
						rows[i].Dotfiles, rows[i].Ferry = checkCompanions(cmd.Context(), t, repo)
					}()
				}
				wg.Wait()
			}
			errText := map[string]string{}
			for tool, err := range listErrs {
				errText[tool] = firstLine(err.Error())
			}
			if o.json {
				rep := map[string]any{"machines": rows}
				if len(errText) > 0 {
					rep["errors"] = errText
				}
				return writeJSON(cmd.OutOrStdout(), rep)
			}
			for _, tool := range companion.RemoteTools {
				if e, ok := errText[tool]; ok {
					fmt.Fprintf(cmd.ErrOrStderr(), "! %s machine list unavailable: %s\n", tool, e)
				}
			}
			out := cmd.OutOrStdout()
			if len(rows) == 0 {
				fmt.Fprintln(out, "No fleet machines yet: add one with `lazylongarm worker add`.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			header := "MACHINE\tSSH\tHC\tHERDR"
			if !noRemote {
				header += "\tDOTFILES\tREV\tBUNDLE\tFERRY"
			}
			fmt.Fprintln(tw, header)
			missing, setup, ferryLogin := false, false, false
			for _, r := range rows {
				line := fmt.Sprintf("%s\t%s\t%s\t%s", r.Name, r.SSHHost, r.Remotes[companion.HC], r.Remotes[companion.Herdr])
				missing = missing || r.Remotes[companion.HC] == "missing" || r.Remotes[companion.Herdr] == "missing"
				if d := r.Dotfiles; d != nil {
					state := d.State
					if d.Error != "" {
						state += ": " + firstLine(d.Error)
					}
					line += fmt.Sprintf("\t%s\t%s\t%s", state, dash(d.Rev), dash(d.Bundle))
					setup = setup || d.State == "not-set-up" || d.State == "chezmoi-only"
				}
				if fs := r.Ferry; fs != nil {
					cell := fs.Summary
					if fs.Error != "" {
						cell += ": " + firstLine(fs.Error)
					}
					line += "\t" + cell
					for _, st := range fs.Backends {
						ferryLogin = ferryLogin || st == "login-needed"
					}
				}
				fmt.Fprintln(tw, line)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if missing {
				fmt.Fprintln(out, "\nRegister the missing ones: lazylongarm companion remotes --all")
			}
			if setup {
				fmt.Fprintln(out, "Set up dotfiles:           lazylongarm companion dotfiles <machine>")
			}
			if vs, old := ferryVersions(rows); len(vs) > 1 || len(old) > 0 {
				why := "versions differ: " + strings.Join(vs, ", ")
				if len(old) > 0 {
					why = "older than " + companion.MinFerryVersion + " on " + strings.Join(old, ", ")
				}
				fmt.Fprintf(out, "Align ferry (%s): lazylongarm companion ferry setup <machine>… --upgrade\n", why)
			}
			if ferryLogin {
				fmt.Fprintln(out, "Sign ferry backends in:    lazylongarm companion ferry setup <machine> --backend <backend>")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&noRemote, "no-remote", false, "skip the dotfiles and ferry checks over SSH")
	return c
}

// checkCompanions reads a machine's dotfiles and ferry state over one SSH
// connection.
func checkCompanions(ctx context.Context, t config.Target, repo string) (*dotfilesStatus, *ferryStatus) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s, err := flows.ReadCompanionState(cctx, plan.NewEnv(), t.SSHHost)
	switch {
	case connection.IsAuthRequired(err):
		msg := "ssh login needed (run: ssh " + t.SSHHost + ")"
		return &dotfilesStatus{State: "error", Bundle: t.DotfilesBundle, Error: msg}, &ferryStatus{State: "error", Summary: "error", Error: msg}
	case err != nil:
		return &dotfilesStatus{State: "error", Bundle: t.DotfilesBundle, Error: err.Error()}, &ferryStatus{State: "error", Summary: "error", Error: err.Error()}
	}
	return dotfilesFrom(s.Dotfiles, s.DotfilesErr, t, repo), ferryFrom(s.Ferry, s.FerryErr)
}

func dotfilesFrom(s companion.DotfilesState, err error, t config.Target, repo string) *dotfilesStatus {
	d := &dotfilesStatus{Bundle: t.DotfilesBundle}
	switch {
	case err != nil:
		d.State, d.Error = "error", err.Error()
	case s.Initialized() && repo != "" && s.Origin != "" && !companion.SameRepo(s.Origin, repo):
		d.State = "other-repo"
	case s.Initialized():
		d.State = "ok"
	case s.Chezmoi != "":
		d.State = "chezmoi-only"
	default:
		d.State = "not-set-up"
	}
	d.Origin, d.Rev = s.Origin, s.Rev
	return d
}

// ---- companion remotes ----

func companionRemotesCommand(o *options) *cobra.Command {
	var all, switchEntries bool
	var tools []string
	var transport, include, hubID string
	var ao applyOptions
	c := &cobra.Command{
		Use:   "remotes [machine...]",
		Short: "Register fleet machines in hc and herdr on this computer",
		Long: `For each machine that hc or herdr does not list yet, run
` + "`hc machine add <ssh-host> --install --name <machine>`" + ` (copies hc there when needed) and
` + "`herdr machine add <ssh-host> --label <machine>`" + ` (installs and starts the herdr
server there; it may ask you to approve that in the terminal).

--transport longarm registers longarm-<worker> instead of the SSH host: ssh
then reaches the worker's built-in sshd through ` + "`longarm proxy`" + ` (keyless, over
the hub's mTLS channel, no direct route or open port needed), so machines
you cannot ssh to directly work too. The aliases come from
` + "`companion ssh-config`" + ` (planned first when ssh does not resolve them yet).
A machine already registered over its SSH host is left alone unless
--switch replaces that entry (its terminals keep running).

Machines are lazylongarm targets (by name) or SSH hosts (with --transport
longarm: Longarm worker names). Bare in a terminal it offers a picker with the
unregistered machines selected. Machines already registered are left alone,
so reruns are safe.`,
		Example: `  lazylongarm companion remotes
  lazylongarm companion remotes --all --tool hc
  lazylongarm companion remotes worker-1 --dry-run --json
  lazylongarm companion remotes worker-1 --transport longarm
  lazylongarm companion remotes --all --transport longarm --switch --tool herdr`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.load(); err != nil {
				return err
			}
			tools, err := validTools(tools)
			if err != nil {
				return err
			}
			viaLongarm := transport == flows.TransportLongarm
			if !viaLongarm && transport != flows.TransportSSH {
				return usage("unknown --transport %q (use ssh, longarm)", transport)
			}
			if !viaLongarm && (switchEntries || include != "") {
				return usage("--switch and --include go with --transport longarm")
			}
			lists, listErrs := o.remoteLists(cmd.Context(), tools)
			for _, tool := range tools {
				if err := listErrs[tool]; err != nil {
					return fmt.Errorf("%s: %w", tool, err)
				}
			}
			unregistered := func(t config.Target) bool {
				for _, list := range lists {
					if viaLongarm {
						if _, ok := companion.FindLongarm(list, targetWorker(t)); !ok {
							return true
						}
					} else if _, ok := companion.Find(list, t.SSHHost, t.Name); !ok {
						return true
					}
				}
				return false
			}
			targets, err := o.pickTargetsAs(cmd, args, all, viaLongarm, "Register which machines in "+strings.Join(tools, " and ")+"?", "Space toggles, enter confirms. Unregistered machines are preselected.", unregistered)
			if err != nil {
				return err
			}
			req := flows.RemotesRequest{Tools: tools, Transport: transport, Switch: switchEntries}
			for _, t := range targets {
				req.Targets = append(req.Targets, flows.RemoteTarget{Name: t.Name, SSHHost: t.SSHHost, Worker: targetWorker(t)})
			}
			deps := flows.RemotesDeps{Tools: o.deps.Companion, Lists: lists}
			if viaLongarm {
				sreq, sdeps, err := o.sshConfigSetup(hubID, "", include)
				if err != nil {
					return err
				}
				for _, t := range req.Targets {
					sreq.Workers = append(sreq.Workers, t.Worker)
				}
				st, err := flows.ReadSSHConfigState(cmd.Context(), sreq, o.deps.Companion)
				if err != nil {
					return err
				}
				req.SSH, deps.SSH, deps.SSHState = sreq, sdeps, st
			}
			p, err := flows.BuildCompanionRemotes(req, deps)
			if err != nil {
				return usage("%v", err)
			}
			return o.runLocalPlan(cmd, p, ao, "Nothing to register.")
		},
	}
	c.Flags().BoolVar(&all, "all", false, "every fleet machine lazylongarm manages")
	c.Flags().StringSliceVar(&tools, "tool", nil, "only these tools: hc, herdr (default: both)")
	c.Flags().StringVar(&transport, "transport", flows.TransportSSH, "how hc/herdr reach the machines: ssh (their SSH hosts) or longarm (longarm-<worker> through Longarm)")
	c.Flags().BoolVar(&switchEntries, "switch", false, "with --transport longarm: replace entries that reach a machine over its SSH host")
	c.Flags().StringVar(&include, "include", "", "with --transport longarm: how the aliases reach ssh, as in `companion ssh-config` (auto, yes, no)")
	c.Flags().StringVar(&hubID, "hub", "", "hub whose Longarm client config to use (default: the default hub)")
	ao.bind(c)
	c.ValidArgsFunction = o.completeMachines(false)
	_ = c.RegisterFlagCompletionFunc("transport", cobra.FixedCompletions([]cobra.Completion{flows.TransportSSH, flows.TransportLongarm}, cobra.ShellCompDirectiveNoFileComp))
	_ = c.RegisterFlagCompletionFunc("tool", cobra.FixedCompletions([]cobra.Completion{companion.HC, companion.Herdr}, cobra.ShellCompDirectiveNoFileComp))
	_ = c.RegisterFlagCompletionFunc("include", cobra.FixedCompletions([]cobra.Completion{"auto", "yes", "no"}, cobra.ShellCompDirectiveNoFileComp))
	return c
}

// runLocalPlan reviews and applies a plan of local steps (dry-run, JSON and
// the empty case included).
func (o *options) runLocalPlan(cmd *cobra.Command, p *plan.Plan, ao applyOptions, nothing string) error {
	if ao.dryRun {
		return o.printPlan(cmd, p)
	}
	if len(p.Steps) == 0 {
		if o.json {
			return writeJSON(cmd.OutOrStdout(), map[string]any{"plan": p.Digest(), "results": []plan.Result{}, "warnings": p.Warnings})
		}
		p.Render(cmd.ErrOrStderr())
		fmt.Fprintln(cmd.OutOrStdout(), nothing)
		return nil
	}
	if err := o.review(cmd, p, ao); err != nil {
		if errors.Is(err, errBack) {
			return wizard.ErrCanceled
		}
		return err
	}
	results, err := o.apply(cmd, p, plan.NewEnv(), ao)
	if o.json {
		_ = writeJSON(cmd.OutOrStdout(), map[string]any{"plan": p.Digest(), "results": results})
	}
	return err
}

// ---- companion ssh-config ----

// sshConfigSetup resolves the alias file, the Include mode and the longarm
// runner for hubID from flags and [companions.ssh].
func (o *options) sshConfigSetup(hubID, file, include string) (flows.SSHConfigRequest, flows.SSHConfigDeps, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return flows.SSHConfigRequest{}, flows.SSHConfigDeps{}, err
	}
	cfg := o.cfg.Companions.SSH
	if file == "" {
		file = cfg.File
	}
	if file == "" {
		file = companion.DefaultAliasFile(home)
	} else if rest, ok := strings.CutPrefix(file, "~/"); ok {
		file = filepath.Join(home, rest)
	}
	if file, err = filepath.Abs(file); err != nil {
		return flows.SSHConfigRequest{}, flows.SSHConfigDeps{}, err
	}
	if include == "" {
		include = cfg.Include
	}
	switch include {
	case "", flows.IncludeAuto, flows.IncludeYes, flows.IncludeNo:
	default:
		return flows.SSHConfigRequest{}, flows.SSHConfigDeps{}, usage("--include must be auto, yes or no, not %q", include)
	}
	configDir := ""
	if h, ok := o.cfg.Hub(hubID); ok {
		configDir = h.ConfigDir
	} else if hubID != "" {
		return flows.SSHConfigRequest{}, flows.SSHConfigDeps{}, usage("no hub %q in %s", hubID, o.cfg.Path())
	}
	run := o.deps.Longarm
	req := flows.SSHConfigRequest{File: file, UserConfig: companion.UserSSHConfig(home), Home: home, Include: include}
	deps := flows.SSHConfigDeps{Tools: o.deps.Companion, Longarm: func(ctx context.Context, args ...string) ([]byte, error) {
		return run(ctx, configDir, args...)
	}}
	return req, deps, nil
}

func companionSSHConfigCommand(o *options) *cobra.Command {
	var all, remove bool
	var file, include, hubID string
	var ao applyOptions
	c := &cobra.Command{
		Use:   "ssh-config [worker...]",
		Short: "Set up OpenSSH aliases longarm-<worker> that reach workers through Longarm",
		Long: `Longarm workers run a built-in sshd that is reachable only through
` + "`longarm proxy <worker>`" + ` over the hub's mTLS channel: keyless, and without a direct
route or an open port. This writes the OpenSSH aliases for it, so plain ssh,
scp, VS Code Remote-SSH, hc and herdr can use ` + "`ssh longarm-<worker>`" + `:

  1. ` + "`longarm ssh-config <worker...>`" + ` (it also records the workers' host keys
     in Longarm's known_hosts) merged into a file lazylongarm owns,
     ~/.ssh/lazylongarm/longarm.conf by default ([companions.ssh] file);
  2. an Include of that file in a marked block at the top of ~/.ssh/config
     (a backup is kept as ~/.ssh/config.lazylongarm.bak). --include no (or
     [companions.ssh] include = "no") never edits ~/.ssh/config and prints the
     line instead; that is also the default when chezmoi manages the file,
     since chezmoi apply would undo the edit (--include yes edits it anyway);
  3. a check that ssh resolves each alias through longarm.

Workers ssh already resolves through longarm are skipped. --remove takes out
the marked block and the alias file and nothing else.

Workers are lazylongarm targets (by name or worker name) or Longarm worker
names; --all is every remote target.`,
		Example: `  lazylongarm companion ssh-config worker-1 worker-2
  lazylongarm companion ssh-config --all --include no
  lazylongarm companion ssh-config --remove`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.load(); err != nil {
				return err
			}
			req, deps, err := o.sshConfigSetup(hubID, file, include)
			if err != nil {
				return err
			}
			if remove {
				if len(args) > 0 || all {
					return usage("--remove takes no workers: it removes the whole alias file and the Include block")
				}
				return o.runLocalPlan(cmd, flows.BuildCompanionSSHConfigRemove(req), ao, "Nothing to remove.")
			}
			targets, err := o.pickTargetsAs(cmd, args, all, true, "Set up ssh aliases for which workers?", "Space toggles, enter confirms.", nil)
			if err != nil {
				return err
			}
			for _, t := range targets {
				req.Workers = append(req.Workers, targetWorker(t))
			}
			st, err := flows.ReadSSHConfigState(cmd.Context(), req, o.deps.Companion)
			if err != nil {
				return err
			}
			p, err := flows.BuildCompanionSSHConfig(req, st, deps)
			if err != nil {
				return usage("%v", err)
			}
			if err := o.runLocalPlan(cmd, p, ao, "ssh already reaches them through longarm."); err != nil {
				return err
			}
			if !ao.dryRun && !o.json && len(p.Steps) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "\nTry: ssh %s\n", companion.LongarmAlias(req.Workers[0]))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&all, "all", false, "every fleet machine lazylongarm manages")
	c.Flags().BoolVar(&remove, "remove", false, "remove the Include block from ~/.ssh/config and delete the alias file")
	c.Flags().StringVar(&file, "file", "", "alias file lazylongarm writes (default: [companions.ssh] file, else ~/.ssh/lazylongarm/longarm.conf)")
	c.Flags().StringVar(&include, "include", "", "how the file reaches ssh: auto (edit ~/.ssh/config unless chezmoi manages it), yes, no (default: [companions.ssh] include, else auto)")
	c.Flags().StringVar(&hubID, "hub", "", "hub whose Longarm client config to use (default: the default hub)")
	ao.bind(c)
	c.ValidArgsFunction = o.completeMachines(true)
	_ = c.RegisterFlagCompletionFunc("include", cobra.FixedCompletions([]cobra.Completion{"auto", "yes", "no"}, cobra.ShellCompDirectiveNoFileComp))
	return c
}

// ---- companion dotfiles ----

type dotfilesFlags struct {
	all                             bool
	repo, bundle, template, answers string
	update                          bool
	apply                           applyOptions
}

func companionDotfilesCommand(o *options) *cobra.Command {
	f := &dotfilesFlags{}
	c := &cobra.Command{
		Use:   "dotfiles [machine...]",
		Short: "Bootstrap (or update) chezmoi dotfiles on fleet machines",
		Long: `Without a bootstrap template it installs chezmoi into ~/.local/bin when missing
and runs ` + "`chezmoi init --apply <repo>`" + `, answering chezmoi's prompts by --answers:

  ask       chezmoi asks each prompt in your terminal (default)
  <preset>  [companions.dotfiles.presets.<preset>] pre-answers prompts, keyed by
            the question text (not the variable name); the rest are asked
            (or take their defaults when there is no terminal)
  defaults  every prompt takes its default (--promptDefaults)

A preset named like the machine's bundle is used automatically. With a template (config
[companions.dotfiles] bootstrap, or --bootstrap) it runs your own command instead,
for example a dotfiles bootstrap script that takes a bundle:

  curl -fsSL https://raw.githubusercontent.com/you/dotfiles/main/bootstrap.sh | bash -s -- --yes --bundle {bundle}

(drop --yes to get that script's own interactive setup in your terminal).
Placeholders {repo} {bundle} {name} {os} {arch} {distro} become shell-quoted
values. The bundle comes from --bundle, the target's recorded bundle,
bundle_by_os for the probed OS, then default_bundle.

The bootstrap runs in your terminal (ssh -tt), so sudo and other prompts work.
A machine that already has dotfiles is skipped (--update pulls and applies);
one with dotfiles from another repository is refused, never replaced.`,
		Example: `  lazylongarm companion dotfiles worker-1
  lazylongarm companion dotfiles --all --update
  lazylongarm companion dotfiles worker-1 --repo you --dry-run`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.load(); err != nil {
				return err
			}
			targets, err := o.pickTargets(cmd, args, f.all, "Set up dotfiles on which machines?", "Space toggles, enter confirms. Machines without recorded dotfiles are preselected.", func(t config.Target) bool { return t.DotfilesRev == "" })
			if err != nil {
				return err
			}
			if err := o.dotfilesSource(cmd, f); err != nil {
				return err
			}
			var dry []any
			for _, t := range targets {
				p, err := o.buildDotfiles(cmd, t, f)
				if err != nil {
					return err
				}
				if f.apply.dryRun {
					if o.json {
						dry = append(dry, p.JSON())
					} else {
						p.Render(cmd.OutOrStdout())
						fmt.Fprintln(cmd.OutOrStdout())
					}
					continue
				}
				if err := o.review(cmd, p, f.apply); err != nil {
					if errors.Is(err, errBack) {
						return wizard.ErrCanceled
					}
					return err
				}
				env := plan.NewEnv()
				results, err := o.apply(cmd, p, env, f.apply)
				if o.json {
					_ = writeJSON(cmd.OutOrStdout(), map[string]any{"machine": t.Name, "plan": p.Digest(), "results": results, "values": env.Values()})
				}
				if err != nil {
					return err
				}
				if !o.json {
					fmt.Fprintf(cmd.OutOrStdout(), "\nDotfiles on %s: %s @ %s\n", t.Name, dash(env.Get("dotfiles_origin")), dash(env.Get("dotfiles_rev")))
				}
			}
			if o.json && f.apply.dryRun {
				if len(dry) == 1 {
					return writeJSON(cmd.OutOrStdout(), dry[0])
				}
				return writeJSON(cmd.OutOrStdout(), dry)
			}
			return nil
		},
	}
	fl := c.Flags()
	fl.BoolVar(&f.all, "all", false, "every fleet machine lazylongarm manages")
	fl.StringVar(&f.repo, "repo", "", "chezmoi source repository: URL, user or user/repo (default: config [companions.dotfiles] repo)")
	fl.StringVar(&f.bundle, "bundle", "", "bundle for {bundle} in the bootstrap template (recorded on the target)")
	fl.StringVar(&f.template, "bootstrap", "", "bootstrap command template (default: config [companions.dotfiles] bootstrap, else built-in chezmoi)")
	fl.BoolVar(&f.update, "update", false, "pull and apply on machines that already have dotfiles")
	fl.StringVar(&f.answers, "answers", "", "how chezmoi's prompts are answered: ask (in the terminal), defaults, or a preset name (default: config answers, a preset named like the bundle, else ask)")
	f.apply.bind(c)
	c.ValidArgsFunction = o.completeMachines(false)
	return c
}

// dotfilesSource fills repo/template from the config, asking for a
// repository in a terminal when neither is set (and offering to save it).
func (o *options) dotfilesSource(cmd *cobra.Command, f *dotfilesFlags) error {
	cfg := o.cfg.Companions.Dotfiles
	if f.repo == "" {
		f.repo = cfg.Repo
	}
	if f.template == "" {
		f.template = cfg.Bootstrap
	}
	if f.repo != "" || f.template != "" || !o.interactive(cmd) || f.apply.dryRun {
		return nil
	}
	values, err := wizard.Edit(cmd.Context(), wizard.Spec{
		Title:       "Which dotfiles?",
		Description: "No repository is configured. chezmoi accepts a URL, a GitHub user (→ user/dotfiles) or user/repo.",
		SubmitLabel: "Continue",
		Fields: []wizard.Field{
			{Key: "repo", Label: "Repository", Required: true, Help: "e.g. https://github.com/you/dotfiles.git"},
			{Key: "save", Label: "Remember it", Kind: wizard.Toggle, Value: "true", Options: []wizard.Choice{{Value: "true", Label: "yes — save as [companions.dotfiles] repo"}, {Value: "false", Label: "no — only this time"}}},
		},
	}, cmd.InOrStdin(), cmd.OutOrStdout())
	if err != nil {
		return err
	}
	f.repo = strings.TrimSpace(values["repo"])
	if values["save"] == "true" {
		if err := o.cfg.Update(func(c *config.Config) error {
			c.Companions.Dotfiles.Repo = f.repo
			return nil
		}); err != nil {
			return err
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "  saved [companions.dotfiles] repo in %s\n", o.cfg.Path())
	}
	return nil
}

func (o *options) buildDotfiles(cmd *cobra.Command, t config.Target, f *dotfilesFlags) (*plan.Plan, error) {
	facts, err := o.probe(cmd, t.SSHHost)
	if err != nil {
		return nil, err
	}
	var state companion.DotfilesState
	if err := o.withAuth(cmd, t.SSHHost, func() error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		s, err := flows.ReadDotfilesState(ctx, plan.NewEnv(), t.SSHHost)
		state = s
		return err
	}); err != nil {
		return nil, err
	}
	cfg := o.cfg.Companions.Dotfiles
	req := flows.DotfilesRequest{
		Host: t.SSHHost, Name: t.Name, Repo: f.repo, Bootstrap: f.template, Update: f.update,
		Bundle:      companion.Bundle(f.bundle, t.DotfilesBundle, cfg.BundleByOS, facts.OS, cfg.DefaultBundle),
		Interactive: o.interactive(cmd),
	}
	answers, err := companion.ResolveAnswers(f.answers, cfg.Answers, req.Bundle, cfg.Presets)
	if err != nil {
		return nil, usage("%v", err)
	}
	req.Answers = answers
	p, err := flows.BuildCompanionDotfiles(req, facts, state, flows.DotfilesDeps{Config: o.cfg})
	if err != nil {
		return nil, usage("%s: %v", t.Name, err)
	}
	return p, nil
}
