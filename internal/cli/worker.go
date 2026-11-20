package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/fleet"
	"github.com/daviddwlee84/lazylongarm/internal/flows"
	"github.com/daviddwlee84/lazylongarm/internal/hostprobe"
	"github.com/daviddwlee84/lazylongarm/internal/longarm"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/release"
	"github.com/daviddwlee84/lazylongarm/internal/wizard"
	"github.com/spf13/cobra"
)

func workerCommand(o *options) *cobra.Command {
	c := &cobra.Command{Use: "worker", Short: "Bootstrap and list Longarm workers", Args: cobra.ArbitraryArgs, RunE: groupHelp}
	c.AddCommand(workerAddCommand(o), workerListCommand(o))
	return c
}

type workerAddFlags struct {
	req         flows.WorkerRequest
	local       bool
	hubID       string
	interactive bool
	apply       applyOptions
}

func workerAddCommand(o *options) *cobra.Command {
	f := &workerAddFlags{}
	c := &cobra.Command{
		Use:   "add [ssh-host]",
		Short: "Make an SSH-reachable machine (or this one) a Longarm worker (wizard)",
		Long: `Copy verified longarmd/longarm binaries to the machine's ~/.local/bin, enroll it
with a one-time code from ` + "`longarm worker add`" + ` (passed over ssh stdin), start the
service, wait until the hub reports it online and run a first command on it.

Bare in a terminal it opens a wizard; with a host argument (or --local) it builds
the plan from flags. Rerunning skips steps that are already in place.`,
		Example: `  lazylongarm worker add
  lazylongarm worker add build-box --label gpu=none
  lazylongarm worker add --local --service user --yes`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.load(); err != nil {
				return err
			}
			if len(args) == 1 {
				f.req.Host = args[0]
			}
			if f.local && f.req.Host != "" {
				return usage("give either an SSH host or --local, not both")
			}
			hub, ok := o.cfg.Hub(f.hubID)
			if !ok {
				return usage("no hub configured: run `lazylongarm hub setup` first")
			}
			f.req.Hub = hub
			if f.req.Version == "" {
				f.req.Version = hub.Version
			}
			bare := f.req.Host == "" && !f.local
			if f.interactive || bare {
				if !o.interactive(cmd) {
					return usage("worker add needs an SSH host argument or --local when not in a terminal")
				}
				return o.workerAddWizard(cmd, f)
			}
			facts, err := o.probe(cmd, f.req.Host)
			if err != nil {
				return err
			}
			p, err := o.buildWorker(cmd.Context(), f.req, facts)
			if err != nil {
				return usage("%v", err)
			}
			if f.apply.dryRun {
				return o.printPlan(cmd, p)
			}
			if err := o.review(cmd, p, f.apply); err != nil {
				if errors.Is(err, errBack) {
					return wizard.ErrCanceled
				}
				return err
			}
			return o.finishWorkerAdd(cmd, p, f)
		},
	}
	fl := c.Flags()
	fl.BoolVar(&f.local, "local", false, "make this machine a worker")
	fl.StringVar(&f.req.Name, "name", "", "worker name (default: the SSH alias, or this machine's hostname)")
	fl.StringArrayVar(&f.req.Labels, "label", nil, "worker label KEY=VALUE (repeatable)")
	fl.StringVar(&f.req.Service, "service", flows.ServiceSystem, "system (service via sudo, survives reboots) | user (background process, no sudo)")
	fl.BoolVar(&f.req.DirectTailnet, "direct-tailnet", false, "also listen on the machine's tailnet IP for direct connections")
	fl.BoolVar(&f.req.Reenroll, "reenroll", false, "enroll again although the machine has a certificate (after a revoke or a lost key)")
	fl.StringVar(&f.req.Mint, "mint", "", "join code source: admin (admin join-code + approve; default for admins) | owner (longarm worker add; needs a Longarm user)")
	fl.StringVar(&f.hubID, "hub", "", "hub id (default: the default hub)")
	fl.StringVar(&f.req.Version, "longarm-version", "", "Longarm release tag to install (default: the hub's recorded version, else latest)")
	fl.BoolVar(&f.interactive, "interactive", false, "open the wizard prefilled from the flags")
	f.apply.bind(c)
	return c
}

func (o *options) buildWorker(ctx context.Context, req flows.WorkerRequest, facts *hostprobe.Facts) (*plan.Plan, error) {
	cacheDir, err := config.CacheDir()
	if err != nil {
		return nil, err
	}
	if req.Version == "" {
		if tag, err := release.Latest(ctx); err == nil {
			req.Version = tag
		}
	}
	client := &longarm.Client{ConfigDir: req.Hub.ConfigDir}
	if req.Mint == "" {
		// Admins mint fleet-worker codes and approve them; owned workers
		// (`longarm worker add`) need a Longarm user record on the hub.
		req.Mint = flows.MintOwner
		wctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		if w, err := client.WhoAmI(wctx); err == nil && w.Role == "admin" {
			req.Mint = flows.MintAdmin
		}
		cancel()
	}
	return flows.BuildWorkerAdd(req, facts, flows.WorkerDeps{
		Config:   o.cfg,
		Client:   client,
		CacheDir: cacheDir,
		Fetch: func(ctx context.Context, tag, asset string, logf func(string, ...any)) (string, error) {
			return release.Fetch(ctx, cacheDir, tag, asset, logf)
		},
	})
}

func (o *options) finishWorkerAdd(cmd *cobra.Command, p *plan.Plan, f *workerAddFlags) error {
	env := plan.NewEnv()
	results, err := o.apply(cmd, p, env, f.apply)
	if o.json {
		_ = writeJSON(cmd.OutOrStdout(), map[string]any{"plan": p.Digest(), "results": results, "values": env.Values()})
	}
	if err != nil {
		return err
	}
	if !o.json {
		out := cmd.OutOrStdout()
		name := flows.WorkerDefaults(f.req, nil).Name
		for _, kv := range p.Summary {
			if kv[0] == "worker name" {
				name = kv[1]
			}
		}
		fmt.Fprintf(out, "\nWorker %s is online (%s).\n\nTry:\n", name, dash(env.Get("worker_path")))
		fmt.Fprintf(out, "  longarm exec %s -- nproc\n", name)
		fmt.Fprintf(out, "  longarm run %s -- sleep 120     # then watch it in the dashboard's Jobs tab\n", name)
		fmt.Fprintf(out, "  lazylongarm                       # dashboard\n")
	}
	return nil
}

func (o *options) workerAddWizard(cmd *cobra.Command, f *workerAddFlags) error {
	ctx := cmd.Context()
	in, out := cmd.InOrStdin(), cmd.OutOrStdout()
	host := f.req.Host
	local := f.local
	for {
		if host == "" && !local {
			const localChoice = "\x00local"
			choices := []wizard.Choice{{Value: localChoice, Label: "this machine"}}
			known := map[string]bool{}
			for _, t := range o.cfg.Targets {
				if t.SSHHost != "" {
					known[t.SSHHost] = true
				}
			}
			for _, h := range suggestions() {
				label := h
				if known[h] {
					label += "  (already a worker)"
				}
				choices = append(choices, wizard.Choice{Value: h, Label: label})
			}
			choices = append(choices, wizard.Choice{Value: "\x00other", Label: "(type another host: alias or user@host)"})
			values, err := wizard.Edit(ctx, wizard.Spec{Title: "Which machine becomes a worker?", Description: "lazylongarm copies verified binaries over SSH, enrolls it with a one-time code and starts the service. Type / to search.", SubmitLabel: "Probe",
				Fields: []wizard.Field{{Key: "host", Label: "Machine", Kind: wizard.Select, Options: choices, Required: true}}}, in, out)
			if err != nil {
				return err
			}
			switch values["host"] {
			case localChoice:
				local = true
			case "\x00other":
				h, err := pickHost(ctx, "Which machine becomes a worker?", "", in, out)
				if err != nil {
					return err
				}
				host = h
			default:
				host = values["host"]
			}
		}
		facts, err := o.probe(cmd, host)
		if err != nil {
			again, cerr := wizard.Confirm(ctx, "Probe failed", fmt.Sprintf("%v\n\nPick another machine?", err), in, out)
			if cerr != nil || !again {
				return err
			}
			host, local = "", false
			continue
		}
		req := flows.WorkerDefaults(f.req, facts)
		req.Host = host
		if req.Name == "" || (f.req.Name == "" && host != "") {
			req.Name = flows.WorkerDefaults(flows.WorkerRequest{Host: host}, facts).Name
		}
		serviceChoices := []wizard.Choice{
			{Value: flows.ServiceSystem, Label: "system service — survives reboots (sudo once" + map[bool]string{true: ", asks for a password", false: ""}[facts.Sudo == "password"] + ")"},
			{Value: flows.ServiceUser, Label: "user process — no sudo; restart it after a reboot"},
		}
		fields := []wizard.Field{
			{Key: "name", Label: "Worker name", Value: req.Name, Required: true},
			{Key: "labels", Label: "Labels", Value: strings.Join(req.Labels, ","), Help: "KEY=VALUE[,KEY=VALUE…], e.g. gpu=3090,pool=home (optional)"},
			{Key: "service", Label: "Run longarmd as", Kind: wizard.Select, Value: req.Service, Options: serviceChoices},
		}
		if facts.TailscaleIP != "" {
			fields = append(fields, wizard.Field{Key: "direct", Label: "Direct over tailnet", Kind: wizard.Toggle, Value: fmt.Sprint(req.DirectTailnet), Options: []wizard.Choice{{Value: "false", Label: "no — reach it through the hub relay (outbound only)"}, {Value: "true", Label: "yes — also listen on " + facts.TailscaleIP + ":7701 for tailnet peers"}}})
		}
		for {
			values, err := wizard.EditDraft(ctx, wizard.Spec{
				Title:       "Worker on " + facts.Where(),
				Description: fmt.Sprintf("%s · %s · %d CPU · %s · sudo %s · hub %s", facts.Distro, facts.Platform(), facts.CPUs, facts.MemSummary(), facts.Sudo, f.req.Hub.Address),
				SubmitLabel: "Review", Back: true, Fields: fields,
			}, in, out)
			if errors.Is(err, wizard.ErrBack) {
				host, local = "", false
				break
			}
			if err != nil {
				return err
			}
			for i := range fields {
				fields[i].Value = values[fields[i].Key]
			}
			req.Name = strings.TrimSpace(values["name"])
			req.Labels = splitLabels(values["labels"])
			req.Service = values["service"]
			req.DirectTailnet = values["direct"] == "true"
			p, err := o.buildWorker(ctx, req, facts)
			if err != nil {
				_ = wizard.Pause(ctx, "Cannot continue: "+err.Error(), in, out)
				continue
			}
			if f.apply.dryRun {
				return o.printPlan(cmd, p)
			}
			if err := o.review(cmd, p, f.apply); err != nil {
				if errors.Is(err, errBack) {
					continue
				}
				return err
			}
			f.req = req
			return o.finishWorkerAdd(cmd, p, f)
		}
	}
}

func suggestions() []string {
	hosts := connection.SuggestHosts()
	sort.Strings(hosts)
	return hosts
}

func splitLabels(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// ---- worker ls ----

func workerListCommand(o *options) *cobra.Command {
	var hubID string
	c := &cobra.Command{
		Use: "ls", Short: "List workers: the hub's registry merged with machines lazylongarm manages", Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			h, err := o.requireHub(hubID)
			if err != nil {
				return err
			}
			client := &longarm.Client{ConfigDir: h.ConfigDir}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			list, lerr := client.Workers(ctx)
			rows := fleet.Merge(o.cfg, list.Nodes)
			if o.json {
				out := map[string]any{"workers": rows}
				if lerr != nil {
					out["hub_error"] = lerr.Error()
				}
				return writeJSON(cmd.OutOrStdout(), out)
			}
			if lerr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "! hub registry unavailable: %v\n", lerr)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "WORKER\tSTATE\tPATH\tPLATFORM\tCPU%\tMEM%\tJOBS\tSSH\tVERSION")
			for _, r := range rows {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.0f\t%.0f\t%d\t%s\t%s\n", r.Name, r.State, dash(r.Path), dash(r.Platform), r.CPU, r.Mem, r.Jobs, dash(r.SSHHost), dash(r.Version))
			}
			return tw.Flush()
		},
	}
	c.Flags().StringVar(&hubID, "hub", "", "hub id")
	return c
}
