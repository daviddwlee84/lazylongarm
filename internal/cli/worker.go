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

	// peer add: also enroll the machine's longarm as a client.
	peer       bool
	clientName string
	clientUser string
}

// noun is what the command makes of the machine.
func (f *workerAddFlags) noun() string {
	if f.peer {
		return "peer"
	}
	return "worker"
}

func workerAddCommand(o *options) *cobra.Command {
	return machineAddCommand(o, &workerAddFlags{}, &cobra.Command{
		Use:   "add [ssh-host]",
		Short: "Make an SSH-reachable machine (or this one) a Longarm worker (wizard)",
		Long: `Install verified longarmd/longarm binaries into the machine's ~/.local/bin,
enroll it with a one-time join code (passed over ssh stdin), start the service,
wait until the hub reports it online and run a first command on it. The
machine's own longarm stays signed out: use ` + "`lazylongarm peer add`" + ` to also let
it dispatch.

By default (--fetch auto) the machine downloads the binaries from GitHub and
checks them against the SHA256SUMS this machine fetched; when that fails, this
machine copies its verified copies over SSH. --fetch push always copies (for
machines without GitHub access); --fetch remote never does.

Bare in a terminal it opens a wizard; with a host argument (or --local) it builds
the plan from flags. Rerunning skips steps that are already in place.`,
		Example: `  lazylongarm worker add
  lazylongarm worker add build-box --label gpu=none
  lazylongarm worker add build-box --fetch push   # build-box cannot reach GitHub
  lazylongarm worker add --local --service user --yes
  lazylongarm worker add build-box --reenroll --owner alice   # fleet worker → owned by alice`,
	})
}

// machineAddCommand wires `worker add` and `peer add`: the same flags and
// flow, peer add with the client enrollment on top.
func machineAddCommand(o *options, f *workerAddFlags, c *cobra.Command) *cobra.Command {
	c.Args = cobra.MaximumNArgs(1)
	c.RunE = func(cmd *cobra.Command, args []string) error {
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
		if !cmd.Flags().Changed("service") {
			// A machine lazylongarm already manages keeps its service mode:
			// re-enrolling a user-mode daemon as a system service would end
			// the daemon and then need sudo to install a unit.
			if t, ok := knownTarget(o.cfg, f.req.Host, f.local); ok && t.Service != "" {
				f.req.Service = t.Service
			}
		}
		bare := f.req.Host == "" && !f.local
		if f.interactive || bare {
			if !o.interactive(cmd) {
				return usage("%s add needs an SSH host argument or --local when not in a terminal", f.noun())
			}
			return o.workerAddWizard(cmd, f)
		}
		facts, err := o.probe(cmd, f.req.Host)
		if err != nil {
			return err
		}
		p, err := o.buildMachine(cmd.Context(), f, f.req, facts)
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
	}
	fl := c.Flags()
	fl.BoolVar(&f.local, "local", false, "make this machine a "+f.noun())
	fl.StringVar(&f.req.Name, "name", "", "worker name (default: the SSH alias, or this machine's hostname)")
	fl.StringArrayVar(&f.req.Labels, "label", nil, "worker label KEY=VALUE (repeatable)")
	fl.StringVar(&f.req.Service, "service", flows.ServiceSystem, "system (service via sudo, survives reboots) | user (background process, no sudo)")
	fl.BoolVar(&f.req.DirectTailnet, "direct-tailnet", false, "also listen on the machine's tailnet IP for direct connections")
	fl.BoolVar(&f.req.Reenroll, "reenroll", false, "enroll again although the machine has a certificate: revokes the hub's node of that name, keeps its labels and restarts the service")
	fl.StringVar(&f.req.Owner, "owner", "", "make it a worker owned by this Longarm user (admin join code with --owner; with --reenroll it moves a fleet worker to the user)")
	if f.peer {
		fl.StringVar(&f.clientName, "client-name", "", "client node name (default: <worker name>-client; node names are unique on the hub)")
		fl.StringVar(&f.clientUser, "client-user", "", "Longarm user the client certificate belongs to (required once the hub separates users)")
	}
	fl.StringVar(&f.req.Fetch, "fetch", flows.FetchAuto, "how the binaries reach the machine: auto (it downloads them from GitHub, else push) | push (copy from here over ssh; no GitHub access needed there) | remote (it downloads them, no fallback)")
	fl.StringVar(&f.req.Mint, "mint", "", "join code source: admin (admin join-code + approve; default for admins) | owner (longarm worker add; needs a Longarm user)")
	fl.StringVar(&f.hubID, "hub", "", "hub id (default: the default hub)")
	fl.StringVar(&f.req.Version, "longarm-version", "", "Longarm release tag to install (default: the hub's recorded version, else latest)")
	fl.BoolVar(&f.interactive, "interactive", false, "open the wizard prefilled from the flags")
	f.apply.bind(c)
	return c
}

func (o *options) buildMachine(ctx context.Context, f *workerAddFlags, req flows.WorkerRequest, facts *hostprobe.Facts) (*plan.Plan, error) {
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
	var who longarm.WhoAmI
	if req.Mint == "" || f.peer || req.Owner != "" || req.Reenroll {
		wctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		who, _ = client.WhoAmI(wctx)
		cancel()
	}
	if req.Mint == "" {
		// Admins mint fleet-worker codes and approve them; owned workers
		// (`longarm worker add`) need a Longarm user record on the hub.
		req.Mint = flows.MintOwner
		if who.Role == "admin" {
			req.Mint = flows.MintAdmin
		}
	}
	if req.Name == "" {
		// The default name (SSH alias / hostname) is needed to find the
		// node a --reenroll replaces.
		req.Name = flows.WorkerDefaults(flows.WorkerRequest{Host: req.Host}, facts).Name
	}
	if err := resolveOwnerAndReplace(ctx, client, who, &req); err != nil {
		return nil, err
	}
	deps := flows.WorkerDeps{
		Config:   o.cfg,
		Client:   client,
		CacheDir: cacheDir,
		Fetch: func(ctx context.Context, tag, asset string, logf func(string, ...any)) (string, error) {
			return release.Fetch(ctx, cacheDir, tag, asset, logf)
		},
		Checksum: func(ctx context.Context, tag, asset string) (string, error) {
			return release.Checksum(ctx, cacheDir, tag, asset)
		},
	}
	if f.peer {
		pr := flows.PeerRequest{WorkerRequest: req, ClientName: f.clientName, ClientUser: f.clientUser, Tenancy: who.Tenancy}
		if who.Tenancy && f.clientUser != "" {
			// A user-bound code only enrolls a user's first device.
			uctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			users, err := client.AdminUsers(uctx)
			cancel()
			if err == nil {
				if u, ok := users.Find(f.clientUser); ok {
					pr.ClientUserDevices = u.DeviceCount
				}
			}
		}
		return flows.BuildPeerAdd(pr, facts, deps)
	}
	return flows.BuildWorkerAdd(req, facts, deps)
}

// knownTarget is the saved target of this SSH host (or of this machine).
func knownTarget(cfg *config.Config, host string, local bool) (config.Target, bool) {
	for _, t := range cfg.Targets {
		if (host != "" && t.SSHHost == host) || (local && host == "" && t.SSHHost == "") {
			if t.Service == flows.ServiceSystem || t.Service == flows.ServiceUser {
				return t, true
			}
		}
	}
	return config.Target{}, false
}

// resolveOwnerAndReplace finds, for --reenroll, the node the machine
// replaces (keeping its labels and its owner: re-enrolling an owned worker
// without --owner must not turn it into a fleet worker every user may use),
// then checks the owner against the hub.
func resolveOwnerAndReplace(ctx context.Context, client *longarm.Client, who longarm.WhoAmI, req *flows.WorkerRequest) error {
	if req.Reenroll && req.Name != "" {
		wctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		list, err := client.Workers(wctx)
		cancel()
		if err != nil {
			return fmt.Errorf("look up worker %s on the hub: %w", req.Name, err)
		}
		for _, n := range list.Nodes {
			if n.Name != req.Name || !n.IsWorker() || n.State == "revoked" {
				continue
			}
			if req.Owner != "" && n.Owner == req.Owner {
				return fmt.Errorf("worker %s is already owned by %s; nothing to move (to enroll it again anyway: `longarm admin revoke %s`, then --reenroll)", req.Name, req.Owner, req.Name)
			}
			if req.Owner == "" && n.Owner != "" {
				if req.Mint == flows.MintOwner && n.Owner != who.User {
					return fmt.Errorf("worker %s is owned by %s; `longarm worker add` would make it %s's: pass --owner %s with an admin certificate", req.Name, n.Owner, dash(who.User), n.Owner)
				}
				if req.Mint == flows.MintAdmin {
					req.Owner = n.Owner // keep it owned
				}
			}
			node := n
			req.Replace = &node
			if len(req.Labels) == 0 {
				req.Labels = carriedLabels(n.Labels)
			}
			break
		}
	}
	if req.Owner == "" {
		return nil
	}
	if who.User != "" && who.User != req.Owner {
		return fmt.Errorf("this machine's longarm acts as user %q: a worker owned by %q would be invisible to it, and admins are not exempt on owned workers; switch first (`lazylongarm user self %s`)", who.User, req.Owner, req.Owner)
	}
	uctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	users, err := client.AdminUsers(uctx)
	cancel()
	if err != nil {
		return fmt.Errorf("check user %q on the hub: %w", req.Owner, err)
	}
	if _, ok := users.Find(req.Owner); !ok {
		return fmt.Errorf("the hub has no user %q (owners are users): `lazylongarm user self %s` creates yours", req.Owner, req.Owner)
	}
	return nil
}

// carriedLabels are a replaced node's labels minus the ones every worker
// reports itself (os, arch) and the hub's own (longarm.io/...).
func carriedLabels(m map[string]string) []string {
	var out []string
	for k, v := range m {
		if k == "os" || k == "arch" || strings.HasPrefix(k, "longarm.io/") {
			continue
		}
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
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
		if f.peer {
			client := flows.PeerDefaults(flows.PeerRequest{WorkerRequest: flows.WorkerRequest{Name: name}, ClientName: f.clientName}, nil).ClientName
			fmt.Fprintf(out, "  longarm exec %s -- sh -lc 'longarm ls'   # its own longarm sees the fleet (client %s)\n", name, client)
			fmt.Fprintf(out, "  longarm admin revoke %s               # take the client certificate back\n", client)
		}
		fmt.Fprintf(out, "  longarm exec %s -- nproc\n", name)
		fmt.Fprintf(out, "  longarm run %s -- sleep 120     # then watch it in the dashboard's Jobs tab\n", name)
		fmt.Fprintf(out, "  lazylongarm                       # dashboard\n")
		if f.req.Host != "" {
			fmt.Fprintf(out, "\nCompanions (optional):\n")
			fmt.Fprintf(out, "  lazylongarm companion remotes %s    # register it in hc / herdr here\n", name)
			fmt.Fprintf(out, "  lazylongarm companion dotfiles %s   # bootstrap your chezmoi dotfiles there\n", name)
		}
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
			values, err := wizard.Edit(ctx, wizard.Spec{Title: "Which machine becomes a " + f.noun() + "?", Description: "lazylongarm installs verified binaries, enrolls it with a one-time code and starts the service" + map[bool]string{true: "; its longarm is enrolled as a client too", false: ""}[f.peer] + ". Type / to search.", SubmitLabel: "Probe",
				Fields: []wizard.Field{{Key: "host", Label: "Machine", Kind: wizard.Select, Options: choices, Required: true}}}, in, out)
			if err != nil {
				return err
			}
			switch values["host"] {
			case localChoice:
				local = true
			case "\x00other":
				h, err := pickHost(ctx, "Which machine becomes a "+f.noun()+"?", "", in, out)
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
		if f.peer {
			fields = append(fields, wizard.Field{Key: "client", Label: "Client name", Value: f.clientName, Help: "its longarm's node name (default: <worker name>-client)"})
		}
		if host != "" {
			fields = append(fields, wizard.Field{Key: "fetch", Label: "Binaries", Kind: wizard.Select, Value: req.Fetch, Options: []wizard.Choice{
				{Value: flows.FetchAuto, Label: "auto — it downloads them from GitHub (checked here); else copy from this machine"},
				{Value: flows.FetchPush, Label: "push — copy from this machine over SSH (no GitHub access needed there)"},
				{Value: flows.FetchRemote, Label: "remote — it downloads them from GitHub; fail if it cannot"},
			}})
		}
		if facts.TailscaleIP != "" {
			fields = append(fields, wizard.Field{Key: "direct", Label: "Direct over tailnet", Kind: wizard.Toggle, Value: fmt.Sprint(req.DirectTailnet), Options: []wizard.Choice{{Value: "false", Label: "no — reach it through the hub relay (outbound only)"}, {Value: "true", Label: "yes — also listen on " + facts.TailscaleIP + ":7701 for tailnet peers"}}})
		}
		for {
			values, err := wizard.EditDraft(ctx, wizard.Spec{
				Title:       map[bool]string{true: "Peer", false: "Worker"}[f.peer] + " on " + facts.Where(),
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
			if v := values["fetch"]; v != "" {
				req.Fetch = v
			}
			req.DirectTailnet = values["direct"] == "true"
			if f.peer {
				f.clientName = strings.TrimSpace(values["client"])
			}
			p, err := o.buildMachine(ctx, f, req, facts)
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
			fmt.Fprintln(tw, "WORKER\tSTATE\tOWNER\tPATH\tPLATFORM\tCPU%\tMEM%\tJOBS\tSSH\tVERSION\tLABELS")
			for _, r := range rows {
				owner := r.Owner
				if owner == "" && r.InHub {
					owner = "fleet"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%.0f\t%.0f\t%d\t%s\t%s\t%s\n", r.Name, r.State, dash(owner), dash(r.Path), dash(r.Platform), r.CPU, r.Mem, r.Jobs, dash(r.SSHHost), dash(r.Version), dash(r.LabelText()))
			}
			return tw.Flush()
		},
	}
	c.Flags().StringVar(&hubID, "hub", "", "hub id")
	return c
}
