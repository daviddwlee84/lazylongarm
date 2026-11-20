package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/cloudfw"
	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/flows"
	"github.com/daviddwlee84/lazylongarm/internal/hubhost"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/secret"
	"github.com/daviddwlee84/lazylongarm/internal/wizard"
	"github.com/spf13/cobra"
)

type panelAccessFlags struct {
	hubID, domain, acmeEmail, authKeyFile string
	noRedirect                            bool
	apply                                 applyOptions
}

func hubPanelAccessCommand(o *options) *cobra.Command {
	f := &panelAccessFlags{}
	c := &cobra.Command{
		Use:   "panel-access [loopback|tailnet|public]",
		Short: "Choose how the web panel is reached: SSH tunnel only, your tailnet, or public HTTPS",
		Long: `The panel always listens on loopback on the hub host (reach it with
` + "`lazylongarm hub panel`" + `). panel-access adds one more way in:

  loopback  SSH tunnel only (the default; undoes the other modes)
  tailnet   install Tailscale on the hub host if needed, log it in (tailscale up
            prints a URL to open) and run ` + "`tailscale serve`" + `: HTTPS at
            https://<hub>.<tailnet>.ts.net/, reachable only from your tailnet
  public    longarm-hub serves the panel over HTTPS itself (Let's Encrypt) on
            port 443 of a public hub; anyone can reach the login page, which
            is then protected by the panel password + TOTP alone

Bare in a terminal it asks. Every run shows a Review first (skip with --yes);
rerunning skips what is already in place.`,
		Example: `  lazylongarm hub panel-access
  lazylongarm hub panel-access tailnet
  lazylongarm hub panel-access public --domain hub.example.com --acme-email ops@example.com --dry-run
  lazylongarm hub panel-access loopback --yes`,
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{config.PanelLoopback, config.PanelTailnet, config.PanelPublic},
		RunE: func(cmd *cobra.Command, args []string) error {
			h, err := o.requireHub(f.hubID)
			if err != nil {
				return err
			}
			if h.SSHHost == "" {
				return usage("hub %s has no SSH host; panel access is configured on the hub machine", h.ID)
			}
			req := flows.PanelAccessRequest{Hub: h, Domain: f.domain, ACMEEmail: f.acmeEmail, Redirect: !f.noRedirect}
			if len(args) == 1 {
				switch args[0] {
				case config.PanelLoopback, config.PanelTailnet, config.PanelPublic:
					req.Mode = args[0]
				default:
					return usage("unknown panel access mode %q: use loopback, tailnet or public", args[0])
				}
			}
			if f.authKeyFile != "" {
				data, err := os.ReadFile(f.authKeyFile)
				if err != nil {
					return err
				}
				req.AuthKey = secret.New(string(data))
			}
			if req.Mode == "" {
				if !o.interactive(cmd) {
					return usage("give the mode: lazylongarm hub panel-access loopback|tailnet|public")
				}
				if err := askPanelAccess(cmd, h, &req); err != nil {
					return err
				}
			}
			return o.runPanelAccess(cmd, req, f.apply)
		},
	}
	fl := c.Flags()
	fl.StringVar(&f.hubID, "hub", "", "hub id")
	fl.StringVar(&f.domain, "domain", "", "public: DNS name for the certificate (default: the hub address's host)")
	fl.StringVar(&f.acmeEmail, "acme-email", "", "public: Let's Encrypt contact for expiry notices (optional)")
	fl.BoolVar(&f.noRedirect, "no-redirect", false, "public: do not serve the http :80 -> https redirect (and keep port 80 closed)")
	fl.StringVar(&f.authKeyFile, "auth-key-file", "", "tailnet: log in with a Tailscale auth key read from this file instead of a browser")
	f.apply.bind(c)
	return c
}

func askPanelAccess(cmd *cobra.Command, h *config.Hub, req *flows.PanelAccessRequest) error {
	values, err := wizard.Edit(cmd.Context(), wizard.Spec{
		Title:       "Web panel access · hub " + h.ID,
		Description: "Now: " + panelNow(h) + ". The SSH tunnel keeps working in every mode.",
		SubmitLabel: "Review",
		Fields: []wizard.Field{
			{Key: "mode", Label: "Reach the panel via", Kind: wizard.Select, Value: h.PanelMode(), Options: []wizard.Choice{
				{Value: config.PanelLoopback, Label: "SSH tunnel only — the safest; lazylongarm hub panel / p"},
				{Value: config.PanelTailnet, Label: "my tailnet — tailscale serve, HTTPS for my devices (phone too)"},
				{Value: config.PanelPublic, Label: "public HTTPS — anyone can reach the login (password + TOTP)"},
			}},
			{Key: "domain", Label: "Public: certificate name", Value: orDefault(req.Domain, flows.DefaultPanelDomain(h)), Help: "DNS name pointing at the hub (public mode only)"},
			{Key: "email", Label: "Public: Let's Encrypt e-mail", Value: req.ACMEEmail, Help: "optional; expiry notices"},
			{Key: "redirect", Label: "Public: http → https", Kind: wizard.Toggle, Value: strconv.FormatBool(req.Redirect), Options: []wizard.Choice{{Value: "true", Label: "yes — also open port 80 for the redirect"}, {Value: "false", Label: "no — port 443 only"}}},
		},
	}, cmd.InOrStdin(), cmd.OutOrStdout())
	if err != nil {
		return err
	}
	req.Mode = values["mode"]
	req.Domain = strings.TrimSpace(values["domain"])
	req.ACMEEmail = strings.TrimSpace(values["email"])
	req.Redirect = values["redirect"] == "true"
	return nil
}

func panelNow(h *config.Hub) string {
	switch h.PanelMode() {
	case config.PanelTailnet:
		return "tailnet (" + orDefault(h.PanelURL(), "tailscale serve") + ")"
	case config.PanelPublic:
		return "public HTTPS (" + h.PanelURL() + ")"
	}
	return "SSH tunnel only"
}

func (o *options) runPanelAccess(cmd *cobra.Command, req flows.PanelAccessRequest, ao applyOptions) error {
	ctx := cmd.Context()
	h := req.Hub
	facts, err := o.probe(cmd, h.SSHHost)
	if err != nil {
		return err
	}
	if req.Mode == config.PanelPublic || h.PanelMode() == config.PanelPublic {
		res, err := connection.Run(ctx, connection.Request{Host: h.SSHHost, Sudo: true, Timeout: 30 * time.Second, Script: `cat "$1"`, Args: []string{hubhost.ConfigPath}})
		if err != nil {
			return fmt.Errorf("read %s on %s (needs passwordless sudo): %w", hubhost.ConfigPath, h.SSHHost, err)
		}
		req.HubConfig = string(res.Stdout)
	}
	deps := flows.PanelAccessDeps{Config: o.cfg}
	if req.Mode == config.PanelPublic && h.Cloud != nil && h.Cloud.Provider == "azure" && h.Cloud.NSG != "" {
		if !o.json {
			fmt.Fprintln(cmd.ErrOrStderr(), "Reading the hub's Azure network security group (az)…")
		}
		if planned, err := planPanelRules(ctx, h.Cloud.NSG, req.Redirect); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "  ! %v (the firewall step plans its rules when it runs)\n", err)
		} else {
			deps.Rules = map[string][]cloudfw.Planned{h.Cloud.NSG: planned}
		}
	}
	p, err := flows.BuildPanelAccess(req, facts, deps)
	if err != nil {
		return usage("%v", err)
	}
	if ao.dryRun {
		return o.printPlan(cmd, p)
	}
	if err := o.review(cmd, p, ao); err != nil {
		if errors.Is(err, errBack) {
			return wizard.ErrCanceled
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
		stored, _ := o.cfg.Hub(h.ID)
		switch req.Mode {
		case config.PanelLoopback:
			fmt.Fprintln(out, "\nThe panel is reachable through the SSH tunnel only: lazylongarm hub panel (p in the dashboard).")
		default:
			fmt.Fprintf(out, "\nPanel: %s\n", stored.PanelURL())
			if req.Mode == config.PanelTailnet {
				fmt.Fprintln(out, "  open it on any device logged in to the same tailnet (install Tailscale on your phone to use it there)")
			} else {
				fmt.Fprintln(out, "  reachable from the Internet; log in with a panel account + TOTP (lazylongarm hub panel-user)")
			}
			fmt.Fprintln(out, "  undo with: lazylongarm hub panel-access loopback")
		}
	}
	return nil
}

func planPanelRules(ctx context.Context, nsg string, redirect bool) ([]cloudfw.Planned, error) {
	rules, err := cloudfw.ListRules(ctx, nsg)
	if err != nil {
		return nil, err
	}
	return cloudfw.PlanRules(rules, cloudfw.PanelRules(redirect))
}
