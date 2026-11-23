package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/colab"
	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/flows"
	"github.com/daviddwlee84/lazylongarm/internal/longarm"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/release"
	"github.com/spf13/cobra"
)

func workerColabCommand(o *options) *cobra.Command {
	c := &cobra.Command{
		Use:   "colab",
		Short: "Borrow Google Colab VMs as temporary workers (add, rm, ls)",
		Long: `Borrow a Google Colab VM as a temporary Longarm worker through the Colab CLI
(uv tool install google-colab-cli): create a session, upload a verified
longarmd, enroll it as a worker owned by you, start it in the background and
wait until it is online. The session has the worker's name.

Colab VMs are disposable: they bill compute units while they run and Colab
reclaims them (at most ~12 h, idle ones sooner). Remove the worker when you
are done: ` + "`lazylongarm worker colab rm <name>`" + ` revokes it and stops the VM.

Colab allows remote shells and distributed-computing workers only with a
positive compute-unit balance (a paid plan, Google AI plans included), so
add refuses to run without one. Never use the VM as a network proxy: Colab
forbids that on every plan.`,
		Args: cobra.ArbitraryArgs, RunE: groupHelp,
	}
	c.AddCommand(workerColabAddCommand(o), workerColabRemoveCommand(o), workerColabListCommand(o))
	return c
}

func (o *options) colabDeps(h *config.Hub) (flows.ColabDeps, *longarm.Client, *colab.Client, error) {
	cacheDir, err := config.CacheDir()
	if err != nil {
		return flows.ColabDeps{}, nil, nil, err
	}
	client := &longarm.Client{ConfigDir: h.ConfigDir}
	cc := &colab.Client{}
	return flows.ColabDeps{
		Client: client,
		Colab:  cc,
		Fetch: func(ctx context.Context, tag, asset string, logf func(string, ...any)) (string, error) {
			return release.Fetch(ctx, cacheDir, tag, asset, logf)
		},
		Checksum: func(ctx context.Context, tag, asset string) (string, error) {
			return release.Checksum(ctx, cacheDir, tag, asset)
		},
	}, client, cc, nil
}

func workerColabAddCommand(o *options) *cobra.Command {
	var (
		req   flows.ColabWorkerRequest
		hubID string
		ao    applyOptions
	)
	c := &cobra.Command{
		Use:   "add [name]",
		Short: "Create a Colab VM and enroll it as a worker owned by you",
		Long: `Create a Colab session (CPU by default, --gpu for an accelerator), install
longarmd (the VM downloads it from GitHub and it must match the release's
SHA256SUMS fetched here; --fetch push uploads a copy verified here instead),
enroll it with a one-time owner code (uploaded as a file, never on a command
line or in the session's execution log), start ` + "`longarmd run`" + ` in the
background and wait until the hub reports the worker online.

The worker is labeled provider=colab (and gpu=<model> with --gpu), so
` + "`longarm run --require provider=colab`" + ` targets it. Rerunning skips
the steps already in place.`,
		Example: `  lazylongarm worker colab add                      # CPU VM, name colab-<mmdd-hhmm>
  lazylongarm worker colab add trainer --gpu T4 --high-mem
  lazylongarm worker colab add --dry-run
  longarm run --on trainer -- python train.py         # then
  lazylongarm worker colab rm trainer`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			h, err := o.requireHub(hubID)
			if err != nil {
				return err
			}
			req.Hub = h
			req.Name = flows.DefaultColabName(time.Now())
			if len(args) == 1 {
				req.Name = args[0]
			}
			deps, _, cc, err := o.colabDeps(h)
			if err != nil {
				return err
			}
			if err := cc.Available(); err != nil {
				return err
			}
			ctx := cmd.Context()
			uctx, cancel := context.WithTimeout(ctx, time.Minute)
			req.Usage, err = cc.Usage(uctx)
			cancel()
			if err != nil {
				return fmt.Errorf("read your Colab balance (is the Colab CLI signed in? run `colab usage`): %w", err)
			}
			if req.Version == "" {
				req.Version = h.Version
			}
			if req.Version == "" {
				if tag, err := release.Latest(ctx); err == nil {
					req.Version = tag
				}
			}
			p, err := flows.BuildColabWorkerAdd(req, deps)
			if err != nil {
				return usage("%v", err)
			}
			if ao.dryRun {
				return o.printPlan(cmd, p)
			}
			if err := o.review(cmd, p, ao); err != nil {
				if errors.Is(err, errBack) {
					return nil
				}
				return err
			}
			env := plan.NewEnv()
			results, err := o.apply(cmd, p, env, ao)
			if o.json {
				_ = writeJSON(cmd.OutOrStdout(), map[string]any{"plan": p.Digest(), "results": results, "values": env.Values()})
			}
			if err != nil {
				return err
			}
			if !o.json {
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "\nColab worker %s is online (%s).\n\nTry:\n", req.Name, dash(env.Get("worker_path")))
				fmt.Fprintf(out, "  longarm exec %s -- nproc\n", req.Name)
				fmt.Fprintf(out, "  longarm run --on %s -- python -c 'import torch; print(torch.cuda.is_available())'\n", req.Name)
				fmt.Fprintf(out, "  longarm forward %s 6006            # e.g. TensorBoard on the VM\n", req.Name)
				fmt.Fprintf(out, "\nWhen done (the VM bills until then):\n  lazylongarm worker colab rm %s\n", req.Name)
			}
			return nil
		},
	}
	fl := c.Flags()
	fl.StringVar(&req.GPU, "gpu", "", "accelerator: T4, L4, G4, H100, A100 (availability depends on your plan; default: a CPU runtime)")
	fl.BoolVar(&req.HighMem, "high-mem", false, "high-RAM machine shape (paid plans)")
	fl.StringVar(&req.Fetch, "fetch", flows.FetchAuto, "how longarmd reaches the VM: auto (the VM downloads it from GitHub, checked against SHA256SUMS fetched here; else upload) | remote (download, no fallback) | push (download here, colab upload)")
	fl.StringArrayVar(&req.Labels, "label", nil, "extra worker label KEY=VALUE (repeatable; provider=colab and gpu= are set for you)")
	fl.StringVar(&hubID, "hub", "", "hub id (default: the default hub)")
	fl.StringVar(&req.Version, "longarm-version", "", "Longarm release tag to install (default: the hub's recorded version, else latest)")
	ao.bind(c)
	return c
}

func workerColabRemoveCommand(o *options) *cobra.Command {
	var (
		hubID string
		ao    applyOptions
	)
	c := &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove"},
		Short:   "Revoke a Colab worker and stop its VM",
		Long: `Revoke the worker on the hub (` + "`longarm worker remove`" + `), then stop the Colab
session of the same name. Either half may already be gone (Colab reclaimed
the VM, or the worker was revoked): the rest still runs. Refuses a worker
without the provider=colab label.`,
		Example: `  lazylongarm worker colab rm trainer
  lazylongarm worker colab rm trainer --yes`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			h, err := o.requireHub(hubID)
			if err != nil {
				return err
			}
			deps, client, cc, err := o.colabDeps(h)
			if err != nil {
				return err
			}
			if err := cc.Available(); err != nil {
				return err
			}
			name := args[0]
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			list, lerr := client.Workers(ctx)
			sessions, serr := cc.Sessions(ctx)
			cancel()
			if lerr != nil {
				return fmt.Errorf("look up worker %s on the hub: %w", name, lerr)
			}
			if serr != nil {
				return fmt.Errorf("list Colab sessions: %w", serr)
			}
			req := flows.ColabRemoveRequest{Name: name}
			for _, n := range list.Nodes {
				if n.Name == name && n.IsWorker() && n.State != "revoked" {
					node := n
					req.Node = &node
				}
			}
			if s, ok := colab.FindSession(sessions, name); ok {
				req.Session = &s
			}
			p, err := flows.BuildColabWorkerRemove(req, deps)
			if err != nil {
				return usage("%v", err)
			}
			if ao.dryRun {
				return o.printPlan(cmd, p)
			}
			if err := o.review(cmd, p, ao); err != nil {
				if errors.Is(err, errBack) {
					return nil
				}
				return err
			}
			env := plan.NewEnv()
			results, err := o.apply(cmd, p, env, ao)
			if o.json {
				_ = writeJSON(cmd.OutOrStdout(), map[string]any{"plan": p.Digest(), "results": results})
			}
			return err
		},
	}
	c.Flags().StringVar(&hubID, "hub", "", "hub id (default: the default hub)")
	ao.bind(c)
	return c
}

// colabRow joins a Colab session and a provider=colab worker of one name.
type colabRow struct {
	Name    string `json:"name"`
	Worker  string `json:"worker"`  // online | offline | "" (not on the hub)
	Session string `json:"session"` // hardware, "" = no session
	Shape   string `json:"shape,omitempty"`
	Jobs    int    `json:"jobs"`
	Labels  string `json:"labels,omitempty"`
	Problem string `json:"problem,omitempty"`
}

func workerColabListCommand(o *options) *cobra.Command {
	var hubID string
	c := &cobra.Command{
		Use: "ls", Short: "List Colab workers and sessions, and your compute-unit balance", Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			h, err := o.requireHub(hubID)
			if err != nil {
				return err
			}
			_, client, cc, err := o.colabDeps(h)
			if err != nil {
				return err
			}
			if err := cc.Available(); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			list, lerr := client.Workers(ctx)
			sessions, serr := cc.Sessions(ctx)
			u, uerr := cc.Usage(ctx)
			if serr != nil {
				return fmt.Errorf("list Colab sessions: %w", serr)
			}
			rows := map[string]*colabRow{}
			for _, s := range sessions {
				rows[s.Name] = &colabRow{Name: s.Name, Session: s.Hardware, Shape: s.Shape}
			}
			for _, n := range list.Nodes {
				if !n.IsWorker() || n.State == "revoked" || n.Labels["provider"] != "colab" {
					continue
				}
				r := rows[n.Name]
				if r == nil {
					r = &colabRow{Name: n.Name}
					rows[n.Name] = r
				}
				r.Worker = map[bool]string{true: "online", false: "offline"}[n.Online]
				r.Jobs = n.JobsActive
				r.Labels = fmtLabels(n.Labels)
			}
			var out []colabRow
			for _, r := range rows {
				switch {
				case r.Worker != "" && r.Session == "":
					r.Problem = "VM gone (reclaimed?): lazylongarm worker colab rm " + r.Name
				case r.Worker == "offline":
					r.Problem = "longarmd not connected"
				case r.Worker == "" && r.Session != "" && lerr == nil:
					r.Problem = "not a worker (another Colab session, or add did not finish)"
				}
				out = append(out, *r)
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
			if o.json {
				res := map[string]any{"rows": out}
				if uerr == nil {
					res["usage"] = map[string]any{"balance": u.Balance, "rate_per_hour": u.RatePerHour, "active": u.Active}
				}
				if lerr != nil {
					res["hub_error"] = lerr.Error()
				}
				return writeJSON(cmd.OutOrStdout(), res)
			}
			if lerr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "! hub registry unavailable: %v\n", lerr)
			}
			if uerr == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Colab: %.2f compute units, burning %.2f/hr across %d VM(s)\n\n", u.Balance, u.RatePerHour, u.Active)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tWORKER\tSESSION\tJOBS\tLABELS\tNOTE")
			for _, r := range out {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", r.Name, dash(r.Worker), dash(joinNonEmpty(r.Session, r.Shape)), r.Jobs, dash(r.Labels), r.Problem)
			}
			return tw.Flush()
		},
	}
	c.Flags().StringVar(&hubID, "hub", "", "hub id")
	return c
}

func fmtLabels(m map[string]string) string {
	var keys []string
	for k := range m {
		if k == "os" || k == "arch" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s := ""
	for i, k := range keys {
		if i > 0 {
			s += ","
		}
		s += k + "=" + m[k]
	}
	return s
}

func joinNonEmpty(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + " " + b
}
