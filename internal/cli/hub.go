package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/cloudfw"
	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/fleet"
	"github.com/daviddwlee84/lazylongarm/internal/flows"
	"github.com/daviddwlee84/lazylongarm/internal/hostprobe"
	"github.com/daviddwlee84/lazylongarm/internal/hubhost"
	"github.com/daviddwlee84/lazylongarm/internal/longarm"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/release"
	"github.com/daviddwlee84/lazylongarm/internal/wizard"
	"github.com/skip2/go-qrcode"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func hubCommand(o *options) *cobra.Command {
	c := &cobra.Command{Use: "hub", Short: "Set up and inspect the longarm-hub you operate", Args: cobra.ArbitraryArgs, RunE: groupHelp}
	c.AddCommand(hubSetupCommand(o), hubStatusCommand(o), hubPanelCommand(o), hubPanelUserCommand(o), hubBackupCommand(o), hubVMCommand(o))
	return c
}

type hubSetupFlags struct {
	req         flows.HubRequest
	cloud       string
	enroll      bool
	interactive bool
	apply       applyOptions
}

func hubSetupCommand(o *options) *cobra.Command {
	f := &hubSetupFlags{}
	c := &cobra.Command{
		Use:   "setup",
		Short: "Install longarm-hub on an SSH-reachable Debian/Ubuntu machine (wizard)",
		Long: `Install and start longarm-hub on a machine you reach over SSH, open its ports
(optionally in the Azure network security group), and enroll this machine as
the hub's admin client.

Bare in a terminal it opens a wizard; with --host and --address it builds the
same plan from flags. Every run shows a Review of the exact commands first
(skip with --yes); rerunning skips steps that are already in place.`,
		Example: `  lazylongarm hub setup
  lazylongarm hub setup --host my-vps --address hub.example.com --cloud azure --dry-run
  lazylongarm hub setup --host my-vps --address hub.example.com --yes --json`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.load(); err != nil {
				return err
			}
			f.req.EnrollLocal = f.enroll
			wantWizard := f.interactive || (f.req.Host == "" && f.req.Address == "")
			if o.json && f.interactive {
				return usage("--interactive cannot be combined with --json")
			}
			if wantWizard {
				if !o.interactive(cmd) {
					return usage("hub setup needs --host and --address when not in a terminal; example: lazylongarm hub setup --host my-vps --address hub.example.com --yes")
				}
				return o.hubSetupWizard(cmd, f)
			}
			if f.req.Host == "" || f.req.Address == "" {
				return usage("hub setup needs both --host and --address (or neither, for the wizard); see --help")
			}
			return o.hubSetupFromFlags(cmd, f)
		},
	}
	fl := c.Flags()
	fl.StringVar(&f.req.Host, "host", "", "SSH alias (or user@host) of the hub machine")
	fl.StringVar(&f.req.Address, "address", "", "DNS name or IP that nodes dial (no port)")
	fl.StringVar(&f.req.Mode, "mode", "", "public | private (default: public, or private when --address is the host's tailnet IP)")
	fl.IntVar(&f.req.Port, "port", 7443, "hub port (tcp + udp; the second STUN port is port+1)")
	fl.StringVar(&f.cloud, "cloud", "auto", "cloud firewall to open: azure | none | auto (azure for *.cloudapp.azure.com when az is installed)")
	fl.BoolVar(&f.enroll, "enroll-local", true, "also install longarm here and join as the hub's admin client")
	fl.BoolVar(&f.req.AutoApprove, "auto-approve", false, "let workers join without `longarm admin approve` (owned workers never need it)")
	fl.StringVar(&f.req.Name, "name", "", "name of this hub in lazylongarm's config (default: the SSH alias)")
	fl.StringVar(&f.req.Version, "longarm-version", "", "Longarm release tag for local binaries (default: latest)")
	fl.BoolVar(&f.interactive, "interactive", false, "open the wizard prefilled from the given flags")
	f.apply.bind(c)
	return c
}

func resolveCloud(choice, address string) string {
	switch choice {
	case "azure":
		return "azure"
	case "none", "":
		return ""
	}
	if strings.HasSuffix(strings.ToLower(address), ".cloudapp.azure.com") {
		if _, err := exec.LookPath("az"); err == nil {
			return "azure"
		}
	}
	return ""
}

func (o *options) hubDeps(ctx context.Context, cmd *cobra.Command, req flows.HubRequest) (flows.HubDeps, error) {
	cacheDir, err := config.CacheDir()
	if err != nil {
		return flows.HubDeps{}, err
	}
	client := &longarm.Client{}
	deps := flows.HubDeps{
		Config: o.cfg,
		Locals: &flows.LocalDeps{CacheDir: cacheDir, Client: client, Install: func(ctx context.Context, env *plan.Env, tag string) (string, error) {
			path, err := flows.InstallLocal(ctx, env, cacheDir, tag)
			if err == nil {
				client.Bin = path
			}
			return path, err
		}},
	}
	if req.Cloud == "azure" {
		fmt.Fprintf(cmd.ErrOrStderr(), "Looking up %s in your Azure subscriptions (az)…\n", req.Address)
		vm, err := cloudfw.FindAzureVM(ctx, req.Address)
		if err != nil {
			return deps, fmt.Errorf("cloud firewall: %w", err)
		}
		deps.AzureVM = vm
		deps.Rules = map[string][]cloudfw.Planned{}
		for _, nsg := range vm.NSGs {
			rules, err := cloudfw.ListRules(ctx, nsg)
			if err != nil {
				return deps, fmt.Errorf("cloud firewall: %w", err)
			}
			planned, err := cloudfw.PlanRules(rules, cloudfw.HubRules(req.Port, req.Port+1))
			if err != nil {
				return deps, fmt.Errorf("cloud firewall: %w", err)
			}
			deps.Rules[nsg] = planned
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "  found VM %s in %s (%s)\n", vm.Name, vm.ResourceGroup, plural(len(vm.NSGs), "NSG"))
	}
	return deps, nil
}

func (o *options) probe(cmd *cobra.Command, host string) (*hostprobe.Facts, error) {
	var facts *hostprobe.Facts
	label := host
	if host == "" {
		label = "this machine"
	}
	err := o.withAuth(cmd, host, func() error {
		if !o.json {
			fmt.Fprintf(cmd.ErrOrStderr(), "Probing %s…\n", label)
		}
		f, err := hostprobe.Probe(cmd.Context(), host)
		facts = f
		return err
	})
	if err != nil {
		return nil, err
	}
	if !o.json {
		fmt.Fprintf(cmd.ErrOrStderr(), "  %s · %s · %d CPU · %s · sudo: %s\n", facts.Distro, facts.Platform(), facts.CPUs, facts.MemSummary(), facts.Sudo)
	}
	return facts, nil
}

func (o *options) hubSetupFromFlags(cmd *cobra.Command, f *hubSetupFlags) error {
	facts, err := o.probe(cmd, f.req.Host)
	if err != nil {
		return err
	}
	f.req.Cloud = resolveCloud(f.cloud, f.req.Address)
	if f.req.Version == "" {
		if tag, err := release.Latest(cmd.Context()); err == nil {
			f.req.Version = tag
		}
	}
	deps, err := o.hubDeps(cmd.Context(), cmd, f.req)
	if err != nil {
		return err
	}
	p, err := flows.BuildHubSetup(f.req, facts, deps)
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
	return o.finishHubSetup(cmd, p, f)
}

func (o *options) finishHubSetup(cmd *cobra.Command, p *plan.Plan, f *hubSetupFlags) error {
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
		fmt.Fprintf(out, "\nHub %s is up.\n", f.req.HubAddress())
		fmt.Fprintf(out, "  CA fingerprint  %s\n", env.Get("ca_fingerprint"))
		fmt.Fprintf(out, "\nNext:\n")
		fmt.Fprintf(out, "  lazylongarm worker add            # bootstrap a worker over SSH (or this machine)\n")
		fmt.Fprintf(out, "  lazylongarm                       # dashboard: workers, jobs, hub\n")
		fmt.Fprintf(out, "  lazylongarm hub panel             # open the web panel through an SSH tunnel\n")
		fmt.Fprintf(out, "  longarm ls                        # Longarm's own view of the fleet\n")
	}
	return nil
}

func (o *options) hubSetupWizard(cmd *cobra.Command, f *hubSetupFlags) error {
	ctx := cmd.Context()
	in, out := cmd.InOrStdin(), cmd.OutOrStdout()
	host := f.req.Host
	for {
		if host == "" {
			h, err := pickHost(ctx, "Where should longarm-hub run?", "A Debian/Ubuntu machine you reach over SSH. Nodes must be able to reach it on the hub port.", in, out)
			if err != nil {
				return err
			}
			host = h
		}
		facts, err := o.probe(cmd, host)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "  probe failed: %v\n", err)
			retry, cerr := wizard.Confirm(ctx, "Probe failed", fmt.Sprintf("%s could not be probed:\n\n%v\n\nPick another host?", host, err), in, out)
			if cerr != nil || !retry {
				return err
			}
			host = ""
			continue
		}
		req := f.req
		req.Host = host
		if req.Address == "" {
			req.Address = connection.ResolveHostname(ctx, host)
			if req.Address == host || req.Address == "" {
				req.Address = facts.TailscaleIP
			}
		}
		if req.Version == "" {
			if tag, err := release.Latest(ctx); err == nil {
				req.Version = tag
			}
		}
		cloudDefault := resolveCloud(f.cloud, req.Address)
		if cloudDefault == "" {
			cloudDefault = "none"
		}
		for {
			values, err := wizard.EditDraft(ctx, wizard.Spec{
				Title:       "Hub on " + host,
				Description: fmt.Sprintf("%s · %s · %d CPU · %s · sudo %s", facts.Distro, facts.Platform(), facts.CPUs, facts.MemSummary(), facts.Sudo),
				SubmitLabel: "Review",
				Back:        true,
				Fields: []wizard.Field{
					{Key: "name", Label: "Hub name (in lazylongarm)", Value: orDefault(req.Name, flows.DefaultHubName(host)), Required: true, Help: "short id used by --hub and the dashboard"},
					{Key: "address", Label: "Address nodes dial", Value: req.Address, Required: true, Help: "DNS name or IP of this machine as seen by workers and clients (port is separate)"},
					{Key: "mode", Label: "Network", Kind: wizard.Select, Value: modeDefault(req, facts), Options: []wizard.Choice{{Value: config.ModePublic, Label: "public — reachable from the Internet (domain / public IP)"}, {Value: config.ModePrivate, Label: "private — only on my network (e.g. tailnet IP)"}}},
					{Key: "port", Label: "Hub port", Value: strconv.Itoa(req.Port), Required: true, Help: "tcp + udp; the second STUN port is port+1"},
					{Key: "cloud", Label: "Cloud firewall", Kind: wizard.Select, Value: cloudDefault, Options: []wizard.Choice{{Value: "azure", Label: "Azure — open the ports in the VM's NSG with az"}, {Value: "none", Label: "none — I manage the firewall myself"}}},
					{Key: "enroll", Label: "Enroll this machine as admin", Kind: wizard.Toggle, Value: strconv.FormatBool(f.enroll), Options: []wizard.Choice{{Value: "true", Label: "yes — install longarm here and join"}, {Value: "false", Label: "no"}}},
					{Key: "approve", Label: "Auto-approve workers", Kind: wizard.Toggle, Value: strconv.FormatBool(req.AutoApprove), Options: []wizard.Choice{{Value: "false", Label: "no — admin approves (owned workers join pre-approved)"}, {Value: "true", Label: "yes"}}},
				},
			}, in, out)
			if errors.Is(err, wizard.ErrBack) {
				host = ""
				break
			}
			if err != nil {
				return err
			}
			req.Name = strings.TrimSpace(values["name"])
			req.Address = strings.TrimSpace(values["address"])
			req.Mode = values["mode"]
			req.Port, _ = strconv.Atoi(strings.TrimSpace(values["port"]))
			req.Cloud = map[string]string{"azure": "azure"}[values["cloud"]]
			req.EnrollLocal = values["enroll"] == "true"
			req.AutoApprove = values["approve"] == "true"
			if err := flows.ValidateHub(flows.HubDefaults(req, facts), facts); err != nil {
				_ = wizard.Pause(ctx, "Cannot continue: "+err.Error(), in, out)
				continue
			}
			deps, err := o.hubDeps(ctx, cmd, req)
			if err != nil {
				skip, cerr := wizard.Confirm(ctx, "Cloud firewall lookup failed", err.Error()+"\n\nContinue without the cloud firewall step?", in, out)
				if cerr != nil || !skip {
					continue
				}
				req.Cloud = ""
				deps, _ = o.hubDeps(ctx, cmd, req)
			}
			p, err := flows.BuildHubSetup(req, facts, deps)
			if err != nil {
				_ = wizard.Pause(ctx, "Cannot build the plan: "+err.Error(), in, out)
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
			return o.finishHubSetup(cmd, p, f)
		}
	}
}

func modeDefault(req flows.HubRequest, f *hostprobe.Facts) string {
	if req.Mode != "" {
		return req.Mode
	}
	if f.TailscaleIP != "" && req.Address == f.TailscaleIP {
		return config.ModePrivate
	}
	return config.ModePublic
}

// pickHost offers ~/.ssh/config aliases plus free text.
func pickHost(ctx context.Context, title, help string, in io.Reader, out io.Writer) (string, error) {
	const other = "\x00other"
	choices := []wizard.Choice{}
	for _, h := range connection.SuggestHosts() {
		choices = append(choices, wizard.Choice{Value: h, Label: h})
	}
	choices = append(choices, wizard.Choice{Value: other, Label: "(type another host: alias or user@host)"})
	values, err := wizard.Edit(ctx, wizard.Spec{Title: title, Description: help + "  Type / to search.", SubmitLabel: "Probe", Fields: []wizard.Field{{Key: "host", Label: "SSH host", Kind: wizard.Select, Options: choices, Required: true}}}, in, out)
	if err != nil {
		return "", err
	}
	if values["host"] != other {
		return values["host"], nil
	}
	values, err = wizard.Edit(ctx, wizard.Spec{Title: title, SubmitLabel: "Probe", Fields: []wizard.Field{{Key: "host", Label: "SSH host", Required: true, Help: "an alias from ~/.ssh/config or user@host"}}}, in, out)
	if err != nil {
		return "", err
	}
	h := strings.TrimSpace(values["host"])
	if err := connection.ValidateHost(h); err != nil {
		return "", usage("%v", err)
	}
	return h, nil
}

func (o *options) requireHub(id string) (*config.Hub, error) {
	if err := o.load(); err != nil {
		return nil, err
	}
	h, ok := o.cfg.Hub(id)
	if !ok {
		if id != "" {
			return nil, usage("no hub %q in %s", id, o.cfg.Path())
		}
		return nil, usage("no hub configured yet: run `lazylongarm hub setup`")
	}
	return h, nil
}

// ---- hub status ----

func hubStatusCommand(o *options) *cobra.Command {
	var hubID string
	c := &cobra.Command{
		Use: "status", Short: "Check the hub service, ports, firewall and this machine's enrollment", Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			h, err := o.requireHub(hubID)
			if err != nil {
				return err
			}
			checks := fleet.HubStatus(cmd.Context(), h)
			if o.json {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"hub": h, "checks": checks})
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "hub %s  %s  (%s, ssh %s)\n", h.ID, h.Address, h.Mode, dash(h.SSHHost))
			for _, ch := range checks {
				mark := map[string]string{"ok": "✓", "warn": "!", "fail": "✗"}[ch.Status]
				fmt.Fprintf(w, "  %s %-12s %s\n", mark, ch.ID, ch.Detail)
				if ch.Fix != "" {
					fmt.Fprintf(w, "    fix: %s\n", ch.Fix)
				}
			}
			return nil
		},
	}
	c.Flags().StringVar(&hubID, "hub", "", "hub id (default: the default hub)")
	return c
}

// ---- hub panel ----

func hubPanelCommand(o *options) *cobra.Command {
	var hubID, createUser string
	var port int
	var noBrowser bool
	c := &cobra.Command{
		Use:   "panel",
		Short: "Open the hub's web panel through an SSH tunnel (Ctrl+C closes it)",
		Long: `The panel listens on loopback on the hub host only. This opens an app-owned
ssh -L tunnel to it, prints the URL and opens a browser; the tunnel lives
until Ctrl+C. --create-user adds a panel login (password + TOTP) first.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			h, err := o.requireHub(hubID)
			if err != nil {
				return err
			}
			if h.SSHHost == "" {
				return usage("hub %s has no SSH host; the panel is only reachable on the hub machine", h.ID)
			}
			if createUser == "" && o.interactive(cmd) {
				if users, err := fleet.PanelUsers(cmd.Context(), h.SSHHost); err == nil && len(users) == 0 {
					name, err := askPanelUser(cmd)
					if err != nil {
						return err
					}
					createUser = name
				}
			}
			if createUser != "" {
				if err := o.createPanelUser(cmd, h, createUser, false); err != nil {
					return err
				}
			}
			t, err := connection.OpenTunnel(cmd.Context(), h.SSHHost, port, orDefault(h.Panel, "127.0.0.1:7480"))
			if err != nil {
				return err
			}
			defer t.Close()
			url := "http://" + t.Address + "/"
			fmt.Fprintf(cmd.OutOrStdout(), "panel: %s  (tunnel via %s; log in with a panel account + TOTP; Ctrl+C to close)\n", url, h.SSHHost)
			if !noBrowser {
				_ = openBrowser(url)
			}
			select {
			case <-cmd.Context().Done():
			case <-t.Done():
				return errors.New("the SSH tunnel closed")
			}
			return nil
		},
	}
	c.Flags().StringVar(&hubID, "hub", "", "hub id")
	c.Flags().IntVar(&port, "port", 7480, "local port (0 = any)")
	c.Flags().BoolVar(&noBrowser, "no-browser", false, "only print the URL")
	c.Flags().StringVar(&createUser, "create-user", "", "create a panel account first (asks for a password; prints the TOTP secret once)")
	return c
}

func hubPanelUserCommand(o *options) *cobra.Command {
	var hubID string
	var reset, remove, list bool
	c := &cobra.Command{
		Use:   "panel-user [name]",
		Short: "Manage web panel logins: list, create, reset (new password + TOTP), remove",
		Long: `Web panel logins use a password plus a TOTP code. Creating or resetting a
login prints the TOTP secret, its otpauth:// URI and a QR code once: scan it
with an authenticator app (Google Authenticator, 1Password, Authy...) and type
the app's 6-digit code into the panel's "TOTP code" field.

Bare in a terminal it shows the existing logins and asks what to do.`,
		Example: `  lazylongarm hub panel-user              # interactive
  lazylongarm hub panel-user --list
  lazylongarm hub panel-user alice          # create
  lazylongarm hub panel-user alice --reset  # new password + new TOTP secret
  lazylongarm hub panel-user alice --remove`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			h, err := o.requireHub(hubID)
			if err != nil {
				return err
			}
			if h.SSHHost == "" {
				return usage("hub %s has no SSH host", h.ID)
			}
			if reset && remove {
				return usage("--reset and --remove are exclusive")
			}
			users, err := fleet.PanelUsers(cmd.Context(), h.SSHHost)
			if err != nil {
				return err
			}
			if list {
				if o.json {
					return writeJSON(cmd.OutOrStdout(), map[string]any{"users": users})
				}
				for _, u := range users {
					fmt.Fprintln(cmd.OutOrStdout(), u)
				}
				return nil
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			} else {
				if !o.interactive(cmd) {
					return usage("give the account name, or --list")
				}
				action, who, err := choosePanelAction(cmd, users)
				if err != nil {
					return err
				}
				name, reset, remove = who, action == "reset", action == "remove"
			}
			exists := false
			for _, u := range users {
				exists = exists || u == name
			}
			switch {
			case remove:
				if !exists {
					return usage("no panel login %q", name)
				}
				if o.interactive(cmd) {
					ok, err := wizard.Confirm(cmd.Context(), "Remove panel login "+name, "Delete the web panel login "+name+" and its sessions on hub "+h.ID+"?\n\nWorkers, jobs and certificates are not affected.", cmd.InOrStdin(), cmd.OutOrStdout())
					if err != nil || !ok {
						return wizard.ErrCanceled
					}
				}
				_, err := connection.Run(cmd.Context(), connection.Request{Host: h.SSHHost, Sudo: true, Timeout: 30 * time.Second,
					Script: `runuser -u longarm-hub -- "$1" --config "$2" user rm "$3"`, Args: []string{hubhost.Binary, hubhost.ConfigPath, name}})
				if err == nil {
					fmt.Fprintf(cmd.OutOrStdout(), "removed panel login %s\n", name)
				}
				return err
			case reset && !exists:
				return usage("no panel login %q to reset", name)
			case !reset && exists:
				return usage("panel login %q exists; use --reset for a new password and TOTP secret", name)
			}
			return o.createPanelUser(cmd, h, name, reset)
		},
	}
	c.Flags().StringVar(&hubID, "hub", "", "hub id")
	c.Flags().BoolVar(&reset, "reset", false, "replace the password and TOTP secret of an existing login")
	c.Flags().BoolVar(&remove, "remove", false, "delete the login")
	c.Flags().BoolVar(&list, "list", false, "list logins")
	return c
}

func choosePanelAction(cmd *cobra.Command, users []string) (string, string, error) {
	choices := []wizard.Choice{{Value: "create", Label: "create a new login"}}
	for _, u := range users {
		choices = append(choices,
			wizard.Choice{Value: "reset\x00" + u, Label: "reset " + u + " (new password + new TOTP secret)"},
			wizard.Choice{Value: "remove\x00" + u, Label: "remove " + u})
	}
	desc := "No logins yet."
	if len(users) > 0 {
		desc = "Existing logins: " + strings.Join(users, ", ") + "."
	}
	values, err := wizard.Edit(cmd.Context(), wizard.Spec{Title: "Web panel logins", Description: desc + " Logins use a password + a TOTP code from an authenticator app.", SubmitLabel: "Continue",
		Fields: []wizard.Field{{Key: "action", Label: "Action", Kind: wizard.Select, Options: choices, Required: true}}}, cmd.InOrStdin(), cmd.OutOrStdout())
	if err != nil {
		return "", "", err
	}
	if action, who, ok := strings.Cut(values["action"], "\x00"); ok {
		return action, who, nil
	}
	name, err := askPanelUser(cmd)
	if err == nil && name == "" {
		err = wizard.ErrCanceled
	}
	return "create", name, err
}

func (o *options) createPanelUser(cmd *cobra.Command, h *config.Hub, name string, reset bool) error {
	if !o.interactive(cmd) {
		return usage("creating a panel login needs a terminal to read the password")
	}
	if config.Slug(name) != strings.ToLower(name) || name == "" {
		return usage("panel user names are lowercase letters, digits and '-'")
	}
	errOut := cmd.ErrOrStderr()
	fmt.Fprintf(errOut, "\nPanel login %q: password + TOTP (two-factor).\n", name)
	fmt.Fprintln(errOut, "  1. choose a password below")
	fmt.Fprintln(errOut, "  2. a QR code appears: scan it with an authenticator app on your phone")
	fmt.Fprintln(errOut, "     (Google Authenticator, 1Password, Authy, Microsoft Authenticator...)")
	fmt.Fprintln(errOut, "  3. log in with user + password + the app's current 6-digit code")
	fmt.Fprintln(errOut)
	var pw []byte
	for {
		fmt.Fprintf(errOut, "New panel password for %s (10+ characters): ", name)
		p1, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(errOut)
		if err != nil {
			return err
		}
		if len(p1) < 10 {
			fmt.Fprintln(errOut, "  too short; use at least 10 characters")
			continue
		}
		fmt.Fprintf(errOut, "Repeat the password: ")
		p2, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(errOut)
		if err != nil {
			return err
		}
		if string(p1) != string(p2) {
			fmt.Fprintln(errOut, "  the passwords differ; try again")
			continue
		}
		pw = p1
		break
	}
	script := `runuser -u longarm-hub -- "$1" --config "$2" user add "$3" --password-stdin`
	if reset {
		script += " --reset"
	}
	res, err := connection.Run(cmd.Context(), connection.Request{Host: h.SSHHost, Sudo: true, Timeout: 30 * time.Second,
		Script: script, Args: []string{hubhost.Binary, hubhost.ConfigPath, name}, Stdin: append(pw, '\n')})
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	uri := ""
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "otpauth "); ok {
			uri = strings.TrimSpace(v)
		}
	}
	verb := map[bool]string{true: "reset", false: "created"}[reset]
	fmt.Fprintf(out, "\npanel login %s %s.\n\n", name, verb)
	if uri != "" {
		if code, err := qrcode.New(uri, qrcode.Medium); err == nil {
			fmt.Fprint(out, code.ToSmallString(false))
		}
	}
	fmt.Fprintf(out, "%s\n\n", strings.TrimSpace(string(res.Stdout)))
	fmt.Fprintln(out, "→ Scan this QR code with your authenticator app now (no camera? add the secret manually).")
	fmt.Fprintln(out, "→ Panel login: user "+name+", your password, and the 6-digit code the app shows (it changes every 30 s).")
	fmt.Fprintln(out, "  The secret is shown only this once; keep it out of chats and logs (reset the login if it leaks).")
	return nil
}

func askPanelUser(cmd *cobra.Command) (string, error) {
	def := config.Slug(os.Getenv("USER"))
	values, err := wizard.Edit(cmd.Context(), wizard.Spec{
		Title:       "Create a web panel account",
		Description: "The account uses a password plus a TOTP code (add the secret to your authenticator). Esc skips.",
		SubmitLabel: "Create",
		Fields:      []wizard.Field{{Key: "name", Label: "Account name", Value: def, Required: true, Help: "lowercase letters, digits and '-'"}},
	}, cmd.InOrStdin(), cmd.OutOrStdout())
	if errors.Is(err, wizard.ErrCanceled) {
		return "", nil
	}
	return strings.TrimSpace(values["name"]), err
}

func openBrowser(url string) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	return c.Start()
}

// ---- hub backup ----

func hubBackupCommand(o *options) *cobra.Command {
	var hubID, outPath string
	var yes bool
	c := &cobra.Command{
		Use:   "backup",
		Short: "Copy the hub's data directory (hub.db, CA) to a local 0600 archive",
		Long: `Stops longarm-hub briefly, streams /var/lib/longarm-hub (without the releases
cache) over SSH into a local tar.gz with mode 0600, then starts the hub again.
The archive contains the CA private key: store it encrypted and offline.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			h, err := o.requireHub(hubID)
			if err != nil {
				return err
			}
			if h.SSHHost == "" {
				return usage("hub %s has no SSH host", h.ID)
			}
			if outPath == "" {
				state, err := config.StateDir()
				if err != nil {
					return err
				}
				dir := filepath.Join(state, "backups")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return err
				}
				outPath = filepath.Join(dir, fmt.Sprintf("longarm-hub-%s-%s.tar.gz", h.ID, time.Now().Format("20060102-150405")))
			}
			if !yes {
				if !o.interactive(cmd) {
					return usage("backup stops the hub for a moment; pass --yes")
				}
				ok, err := wizard.Confirm(cmd.Context(), "Back up hub "+h.ID, fmt.Sprintf("longarm-hub on %s will stop for a few seconds while %s is archived to\n\n  %s (mode 0600, contains the CA private key)\n\nRunning jobs on workers are not affected.", h.SSHHost, hubhost.DataDir, orDefault(outPath, "$XDG_STATE_HOME/lazylongarm/backups/")), cmd.InOrStdin(), cmd.OutOrStdout())
				if err != nil || !ok {
					return wizard.ErrCanceled
				}
			}
			f, err := os.OpenFile(outPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			defer f.Close()
			res, err := connection.Run(cmd.Context(), connection.Request{Host: h.SSHHost, Sudo: true, Timeout: 10 * time.Minute, Limit: 2 << 30,
				Script: `set -eu
systemctl stop longarm-hub
trap 'systemctl start longarm-hub' EXIT
tar -C /var/lib -czf - --exclude=longarm-hub/releases longarm-hub`})
			if err != nil {
				os.Remove(outPath)
				return err
			}
			if _, err := f.Write(res.Stdout); err != nil {
				return err
			}
			abs, _ := filepath.Abs(outPath)
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "wrote %s (%.1f KB, mode 0600); the hub is running again\n", abs, float64(len(res.Stdout))/1024)
			fmt.Fprintln(out, "It contains the hub's CA private key: encrypt it and keep it offline, e.g.")
			fmt.Fprintf(out, "  age -p -o %s.age %s && rm %s\n", abs, abs, abs)
			return nil
		},
	}
	c.Flags().StringVar(&hubID, "hub", "", "hub id")
	c.Flags().StringVarP(&outPath, "out", "o", "", "archive path (default $XDG_STATE_HOME/lazylongarm/backups/longarm-hub-<id>-<time>.tar.gz)")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	return c
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
