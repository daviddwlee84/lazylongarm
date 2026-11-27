package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/companion"
	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/flows"
	"github.com/daviddwlee84/lazylongarm/internal/hostprobe"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/wizard"
	"github.com/spf13/cobra"
)

// ---- companion ferry ----

func companionFerryCommand(o *options) *cobra.Command {
	c := &cobra.Command{
		Use:   "ferry",
		Short: "Move bulk data between fleet machines through a cloud relay (ferry), not the hub",
		Long: `longarm cp/put/get move data directly between machines when they reach each
other, else through the hub's relay, which is rate-limited and billed as the
hub's egress (longarm refuses large relayed copies). ferry
(https://github.com/daviddwlee84/ferry) moves bulk data through cloud storage
both machines can reach instead: Baidu Pan (BaiduPCS-Go), an rclone remote or a
GitHub draft release, resumable and sha256-verified.

  setup  install ferry and the backend CLI on machines, and sign the backend in
         (its own login runs in your terminal; lazylongarm never sees it)
  pair   write the same ferry job on a sender and a receiver, then check both

lazylongarm never starts a transfer unless you pass pair --service, and never
reads, stores or copies backend credentials.`,
		Args: cobra.ArbitraryArgs, RunE: groupHelp,
	}
	c.AddCommand(companionFerrySetupCommand(o), companionFerryPairCommand(o))
	return c
}

type ferrySetupFlags struct {
	all, upgrade, noLogin bool
	backends              []string
	apply                 applyOptions
}

func companionFerrySetupCommand(o *options) *cobra.Command {
	f := &ferrySetupFlags{}
	c := &cobra.Command{
		Use:   "setup [machine...]",
		Short: "Install ferry and backend CLIs on fleet machines and sign the backends in",
		Long: `Installs uv into ~/.local/bin when missing, then ferry with ` + "`uv tool install`" + `
(source: config [companions.ferry] install, else a ferry version pinned by this
lazylongarm, so machines set up together match). A ferry older than ` + companion.MinFerryVersion + ` is
upgraded; --upgrade reinstalls from the source, which aligns machines that
were set up at different times. The
baidupcs backend installs BaiduPCS-Go into ~/.local/bin from its GitHub
releases; rclone and gh must come from the system's package manager.

A backend that is not signed in gets its own login in your terminal (ssh -tt):
BaiduPCS-Go login, rclone config or gh auth login. lazylongarm only checks
whether a backend is signed in (never which account) and records the backend
names on the target. --no-login skips the logins and tells you what to run.`,
		Example: `  lazylongarm companion ferry setup worker-1 worker-2
  lazylongarm companion ferry setup --all --backend rclone
  lazylongarm companion ferry setup worker-1 --upgrade --dry-run`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.load(); err != nil {
				return err
			}
			backends := f.backends
			if len(backends) == 0 {
				backends = o.cfg.Companions.Ferry.Backends
			}
			for _, b := range backends {
				if err := companion.ValidBackend(b); err != nil {
					return usage("%v", err)
				}
			}
			targets, err := o.pickTargets(cmd, args, f.all, "Set up ferry on which machines?", "Space toggles, enter confirms. Machines without ferry recorded are preselected.", func(t config.Target) bool { return t.FerryVersion == "" })
			if err != nil {
				return err
			}
			var dry []any
			for _, t := range targets {
				facts, err := o.probe(cmd, t.SSHHost)
				if err != nil {
					return err
				}
				state, err := o.readFerryState(cmd, t.SSHHost)
				if err != nil {
					return err
				}
				req := flows.FerrySetupRequest{
					Host: t.SSHHost, Name: t.Name, Spec: o.cfg.Companions.Ferry.Install, Backends: backends,
					Upgrade: f.upgrade, Interactive: o.interactive(cmd),
				}
				req.Login = !f.noLogin && req.Interactive
				p, err := flows.BuildCompanionFerrySetup(req, facts, state, flows.FerryDeps{Config: o.cfg})
				if err != nil {
					return usage("%s: %v", t.Name, err)
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
				env, err := o.reviewAndApply(cmd, p, f.apply, map[string]any{"machine": t.Name})
				if err != nil {
					return err
				}
				if !o.json {
					fmt.Fprintf(cmd.OutOrStdout(), "\nferry on %s: %s, signed in: %s\n", t.Name, dash(env.Get("ferry_version")), dash(env.Get("ferry_backends")))
				}
			}
			return writeDry(cmd, o, f.apply, dry)
		},
	}
	fl := c.Flags()
	fl.BoolVar(&f.all, "all", false, "every fleet machine lazylongarm manages")
	fl.StringSliceVar(&f.backends, "backend", nil, "backend CLIs to set up: baidupcs, rclone, gh (repeatable; default: config [companions.ferry] backends, else baidupcs)")
	fl.BoolVar(&f.upgrade, "upgrade", false, "reinstall ferry from its source although it is installed")
	fl.BoolVar(&f.noLogin, "no-login", false, "do not run backend logins; print what to run instead")
	f.apply.bind(c)
	c.ValidArgsFunction = o.completeMachines(false)
	_ = c.RegisterFlagCompletionFunc("backend", cobra.FixedCompletions(companion.FerryBackends, cobra.ShellCompDirectiveNoFileComp))
	return c
}

type ferryPairFlags struct {
	via, name, prefix, glob, remote, repo, tag, deleteSource string
	sshHost, landing                                         string
	extract, service                                         bool
	apply                                                    applyOptions
}

func companionFerryPairCommand(o *options) *cobra.Command {
	f := &ferryPairFlags{}
	c := &cobra.Command{
		Use:   "pair <sender>:<path> <receiver>:<path>",
		Short: "Write the same ferry job on a sender and a receiver, then check both sides",
		Long: `Writes one ferry job file, ~/.config/ferry/jobs/<name>.toml, on both machines
(it refuses to replace a different file), runs ferry doctor on each side and
ferry plan on the sender, and records the pair in lazylongarm's config.

Paths are on each machine: absolute, ~/..., or relative to its home directory.
A directory source sends each entry in it as one unit (--glob narrows that,
relative to the directory); a file source is one unit. The receiver keeps tar
archives under the destination unless --extract. Sources are kept
(delete_source = never) unless --delete-source says otherwise.

--via rsync pushes over ssh from the sender straight to the receiver (it needs
a key from one to the other). Use it only on a direct route (LAN, tailnet): an
ssh alias through ` + "`longarm proxy`" + ` takes the hub's relay when the two workers
have no direct path, and the plan warns when it sees one.

Both machines need the same ferry (at least ` + companion.MinFerryVersion + `) and the relay's backend
signed in: run ` + "`lazylongarm companion ferry setup`" + ` first (--upgrade aligns versions). The transfer starts only with --service
(systemd user units on Linux); otherwise the next steps are printed.`,
		Example: `  lazylongarm companion ferry pair worker-1:~/datasets/run42 worker-2:/srv/inbox
  lazylongarm companion ferry pair worker-1:/data/exports worker-2:~/in --via rclone --remote gdrive: --glob '*.parquet'
  lazylongarm companion ferry pair worker-1:~/out worker-2:~/in --via ghrelease --repo you/relay --dry-run
  lazylongarm companion ferry pair worker-1:~/out nas:/srv/in --via rsync --ssh-host nas.lan`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.load(); err != nil {
				return err
			}
			return o.ferryPair(cmd, f, args)
		},
	}
	fl := c.Flags()
	fl.StringVar(&f.via, "via", "", "relay adapter: baidupcs, rclone, ghrelease or rsync (default: baidupcs)")
	fl.StringVar(&f.name, "name", "", "job name (default: from the source path); also the relay prefix and state directory name")
	fl.StringVar(&f.prefix, "prefix", "", "relay directory for this job (default: /ferry/<name>)")
	fl.StringVar(&f.glob, "glob", "", "units under a directory source, relative to it (default: *)")
	fl.StringVar(&f.remote, "remote", "", "rclone remote both machines have, e.g. gdrive: (--via rclone)")
	fl.StringVar(&f.repo, "repo", "", "GitHub repository owner/name for draft-release parts (--via ghrelease)")
	fl.StringVar(&f.tag, "tag", "", "draft release tag (--via ghrelease; default: ferry-<name>)")
	fl.StringVar(&f.sshHost, "ssh-host", "", "how the sender reaches the receiver over ssh (--via rsync; default: the receiver's SSH host here, which must resolve on the sender too)")
	fl.StringVar(&f.landing, "landing", "", "landing directory on the receiver (--via rsync; default: ~/.cache/ferry/<name>/landing)")
	fl.BoolVar(&f.extract, "extract", false, "unpack units under the destination instead of keeping tar archives")
	fl.StringVar(&f.deleteSource, "delete-source", "never", "never | after_remote_size | after_receiver_ack (ferry deletes sent sources)")
	fl.BoolVar(&f.service, "service", false, "install and start ferry's systemd user services (Linux), receiver first")
	f.apply.bind(c)
	c.ValidArgsFunction = o.completeMachineColon()
	_ = c.RegisterFlagCompletionFunc("via", cobra.FixedCompletions(companion.FerryAdapters, cobra.ShellCompDirectiveNoFileComp))
	_ = c.RegisterFlagCompletionFunc("delete-source", cobra.FixedCompletions([]string{"never", "after_remote_size", "after_receiver_ack"}, cobra.ShellCompDirectiveNoFileComp))
	return c
}

// splitMachinePath splits "machine:path".
func splitMachinePath(arg string) (string, string, error) {
	m, p, ok := strings.Cut(arg, ":")
	if !ok || m == "" || p == "" {
		return "", "", usage("%q: write <machine>:<path>", arg)
	}
	return m, p, nil
}

type ferrySide struct {
	target config.Target
	facts  *hostprobe.Facts
	state  companion.FerryState
	path   string // absolute
}

func (o *options) ferryPair(cmd *cobra.Command, f *ferryPairFlags, args []string) error {
	via := f.via
	if via == "" {
		via = "baidupcs"
	}
	if _, err := companion.AdapterBackend(via); err != nil {
		return usage("%v", err)
	}
	options := map[string]string{}
	switch via {
	case "rclone":
		if f.remote == "" {
			return usage("--via rclone needs --remote, e.g. --remote gdrive:")
		}
		options["remote"] = f.remote
	case "ghrelease":
		if f.repo == "" {
			return usage("--via ghrelease needs --repo owner/name")
		}
		options["repo"] = f.repo
	}
	if (f.sshHost != "" || f.landing != "") && via != "rsync" {
		return usage("--ssh-host and --landing are for --via rsync")
	}
	if f.remote != "" && via != "rclone" {
		return usage("--remote is for --via rclone")
	}
	if (f.repo != "" || f.tag != "") && via != "ghrelease" {
		return usage("--repo and --tag are for --via ghrelease")
	}
	var sides [2]ferrySide
	for i, a := range args {
		m, p, err := splitMachinePath(a)
		if err != nil {
			return err
		}
		ts, err := o.pickTargets(cmd, []string{m}, false, "", "", nil)
		if err != nil {
			return err
		}
		sides[i].target = ts[0]
		sides[i].path = p
	}
	if sides[0].target.SSHHost == sides[1].target.SSHHost {
		return usage("sender and receiver are the same machine (%s)", sides[0].target.Name)
	}
	for i := range sides {
		s := &sides[i]
		facts, err := o.probe(cmd, s.target.SSHHost)
		if err != nil {
			return err
		}
		s.facts = facts
		if s.path, err = companion.ExpandPath(s.path, facts.Home); err != nil {
			return usage("%s: %v", s.target.Name, err)
		}
		if s.state, err = o.readFerryState(cmd, s.target.SSHHost); err != nil {
			return err
		}
	}
	snd, rcv := sides[0], sides[1]

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	kind, err := flows.ReadPathKind(ctx, plan.NewEnv(), snd.target.SSHHost, snd.path)
	cancel()
	if err != nil {
		return err
	}
	root, glob, selected := snd.path, f.glob, ""
	switch kind {
	case "dir":
		if glob == "" {
			glob, selected = "*", "each entry in the directory is one unit"
		}
	case "file":
		if glob != "" {
			return usage("--glob needs a directory source; %s:%s is a file", snd.target.Name, snd.path)
		}
		root, glob, selected = path.Dir(snd.path), path.Base(snd.path), "the one file"
	case "missing":
		return usage("%s:%s does not exist", snd.target.Name, snd.path)
	default:
		return usage("%s:%s is neither a file nor a directory", snd.target.Name, snd.path)
	}
	name := f.name
	if name == "" {
		name = companion.JobNameFrom(snd.path)
	}
	if err := companion.ValidJobName(name); err != nil {
		return usage("%v (pick one with --name)", err)
	}
	prefix := f.prefix
	if prefix == "" {
		prefix = "/ferry/" + name
	}
	if via == "ghrelease" {
		options["tag"] = f.tag
		if f.tag == "" {
			options["tag"] = "ferry-" + name
		}
	}
	job := companion.FerryJob{
		Name: name, Root: root, Glob: glob, SourceSelected: selected, Dest: rcv.path,
		Extract: f.extract, Adapter: via, Prefix: prefix, Options: options,
		DeleteSource: f.deleteSource, GeneratedBy: "lazylongarm " + Version,
	}
	var rsyncHost string
	var route companion.SSHRoute
	if via == "rsync" {
		// The sender runs rsync over ssh to the receiver; the receiver reads
		// the landing directory locally.
		rsyncHost = f.sshHost
		if rsyncHost == "" {
			rsyncHost = rcv.target.SSHHost
		}
		if err := connection.ValidateHost(rsyncHost); err != nil {
			return usage("--ssh-host: %v", err)
		}
		landing := f.landing
		if landing == "" {
			landing = path.Join(rcv.facts.Home, ".cache", "ferry", name, "landing")
		}
		if landing, err = companion.ExpandPath(landing, rcv.facts.Home); err != nil {
			return usage("--landing: %v", err)
		}
		options["host"], options["root"] = rsyncHost, landing
		job.ReceiverAdapter, job.ReceiverOptions = "local", map[string]string{"root": landing}
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		route, err = flows.ReadSSHRoute(ctx, plan.NewEnv(), snd.target.SSHHost, rsyncHost)
		cancel()
		if err != nil {
			return err
		}
	}
	end := func(s ferrySide) flows.FerryEnd {
		return flows.FerryEnd{Host: s.target.SSHHost, Name: s.target.Name, Record: s.target.ID, OS: s.facts.OS, JobPath: companion.JobPath(s.facts.Home, name)}
	}
	req := flows.FerryPairRequest{Job: job, Sender: end(snd), Receiver: end(rcv), Service: f.service, RsyncHost: rsyncHost, RsyncRoute: route}
	p, err := flows.BuildCompanionFerryPair(req, snd.state, rcv.state, flows.FerryDeps{Config: o.cfg})
	if err != nil {
		return usage("%v", err)
	}
	if f.apply.dryRun {
		if o.json {
			return writeJSON(cmd.OutOrStdout(), p.JSON())
		}
		p.Render(cmd.OutOrStdout())
		return nil
	}
	env, err := o.reviewAndApply(cmd, p, f.apply, map[string]any{"job": name})
	if err != nil || o.json {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "\nferry job %s is on %s and %s (%s unit(s) to send).\n", name, snd.target.Name, rcv.target.Name, dash(env.Get("units")))
	sj, rj := req.Sender.JobPath, req.Receiver.JobPath
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if f.service && snd.facts.OS == "linux" && rcv.facts.OS == "linux" {
		fmt.Fprintln(w, "The services are running. Progress:")
	} else {
		fmt.Fprintln(w, "Start it, each on its machine (under pueue/tmux, or `ferry service <job> --side <side> --install` on Linux):")
		fmt.Fprintf(w, "  %s (receiver)\tferry receive %s --follow\n", rcv.target.Name, rj)
		fmt.Fprintf(w, "  %s (sender)\tferry send %s\n", snd.target.Name, sj)
		fmt.Fprintln(w, "Progress:")
	}
	fmt.Fprintf(w, "  %s\tferry status %s\n", snd.target.Name, sj)
	fmt.Fprintf(w, "  %s\tferry status %s --side receiver\n", rcv.target.Name, rj)
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(out, "ferry is in ~/.local/bin, which a non-interactive ssh shell may not have on PATH.")
	return nil
}

// readFerryState reads companion.FerryStateScript on host, offering an SSH
// login when one is needed.
func (o *options) readFerryState(cmd *cobra.Command, host string) (companion.FerryState, error) {
	var state companion.FerryState
	err := o.withAuth(cmd, host, func() error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		s, err := flows.ReadFerryState(ctx, plan.NewEnv(), host)
		state = s
		return err
	})
	return state, err
}

// reviewAndApply shows p for review, applies it and, with --json, prints the
// results next to the extra fields.
func (o *options) reviewAndApply(cmd *cobra.Command, p *plan.Plan, ao applyOptions, extra map[string]any) (*plan.Env, error) {
	if err := o.review(cmd, p, ao); err != nil {
		if errors.Is(err, errBack) {
			return nil, wizard.ErrCanceled
		}
		return nil, err
	}
	env := plan.NewEnv()
	results, err := o.apply(cmd, p, env, ao)
	if o.json {
		rep := map[string]any{"plan": p.Digest(), "results": results, "values": env.Values()}
		for k, v := range extra {
			rep[k] = v
		}
		_ = writeJSON(cmd.OutOrStdout(), rep)
	}
	return env, err
}

// writeDry prints the collected --dry-run --json plans (one object for one
// machine, else a list).
func writeDry(cmd *cobra.Command, o *options, ao applyOptions, dry []any) error {
	if !o.json || !ao.dryRun {
		return nil
	}
	if len(dry) == 1 {
		return writeJSON(cmd.OutOrStdout(), dry[0])
	}
	return writeJSON(cmd.OutOrStdout(), dry)
}

// completeMachineColon completes "<machine>:" for the two pair arguments.
func (o *options) completeMachineColon() completeFunc {
	machines := o.completeMachines(false)
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(args) >= 2 || strings.Contains(toComplete, ":") {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		list, _ := machines(cmd, nil, toComplete)
		var out []cobra.Completion
		for _, c := range list {
			name, desc, _ := strings.Cut(c, "\t")
			if desc != "" {
				out = append(out, name+":\t"+desc)
			} else {
				out = append(out, name+":")
			}
		}
		return slices.Clip(out), cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
	}
}

// ---- status column ----

type ferryStatus struct {
	State    string            `json:"state"` // installed | not-installed | error
	Version  string            `json:"version,omitempty"`
	Backends map[string]string `json:"backends,omitempty"` // backend → signed-in | login-needed | not-installed
	Summary  string            `json:"summary"`
	Error    string            `json:"error,omitempty"`
}

func ferryFrom(s companion.FerryState, err error) *ferryStatus {
	switch {
	case err != nil:
		return &ferryStatus{State: "error", Summary: "error", Error: err.Error()}
	case s.Ferry == "":
		return &ferryStatus{State: "not-installed", Summary: s.Summary()}
	}
	st := &ferryStatus{State: "installed", Version: s.Version, Summary: s.Summary(), Backends: map[string]string{}}
	for _, b := range companion.FerryBackends {
		st.Backends[b] = s.BackendState(b)
	}
	return st
}

// ferryVersions lists the distinct installed ferry versions (sorted) and the
// machines whose ferry is older than companion.MinFerryVersion.
func ferryVersions(rows []companionRow) (versions, outdated []string) {
	seen := map[string]bool{}
	for _, r := range rows {
		if r.Ferry == nil || r.Ferry.State != "installed" {
			continue
		}
		seen[dash(r.Ferry.Version)] = true
		if !companion.VersionAtLeast(r.Ferry.Version, companion.MinFerryVersion) {
			outdated = append(outdated, r.Name)
		}
	}
	return slices.Sorted(maps.Keys(seen)), outdated
}
