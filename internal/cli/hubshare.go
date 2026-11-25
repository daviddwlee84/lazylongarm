package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/flows"
	"github.com/daviddwlee84/lazylongarm/internal/hubshare"
	"github.com/daviddwlee84/lazylongarm/internal/longarm"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/wizard"
	"github.com/spf13/cobra"
)

// ---- hub share ----

func hubShareCommand(o *options) *cobra.Command {
	var hubID string
	req := flows.ShareRequest{}
	var ao applyOptions
	c := &cobra.Command{
		Use:   "share <ssh-host>",
		Short: "Register this hub on another operator machine (its longarm already joined)",
		Long: `Make another machine you operate from (a second laptop, a desktop at work) a
full lazylongarm console for this hub: Hub tab, hub status, panel, panel logins.

The machine's longarm must already be joined to the same hub (checked: hub
address and CA fingerprint); ` + "`lazylongarm peer add <host>`" + ` does that. Then:

  1. pin the hub host's SSH key there, copied from this machine's known_hosts
     (never ssh-keyscan),
  2. let it ssh to the hub host: add its existing default public key (or the
     --identity key) to the hub host's authorized_keys (shown in the review;
     --no-authorize skips); with --identity, a Host alias for the hub host goes
     into a file its ~/.ssh/config already Includes (never ~/.ssh/config),
  3. register the hub in its lazylongarm config (` + "`hub import --stdin`" + `),
  4. check ` + "`hub status`" + ` from there.

Nothing secret travels: no private keys, no certificates. With --targets the
machines lazylongarm manages here go along, except aliases the other machine
resolves to another host (or not at all). Each machine keeps its own copy:
share again after ` + "`hub panel-access`" + ` or moving the hub.`,
		Example: `  lazylongarm hub share work-laptop --dry-run
  lazylongarm hub share work-laptop --targets
  lazylongarm hub share work-laptop --identity '~/.ssh/work_ed25519'
  lazylongarm hub share work-laptop --yes --json`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeSSHHost,
		RunE: func(cmd *cobra.Command, args []string) error {
			h, err := o.requireHub(hubID)
			if err != nil {
				return err
			}
			if err := connection.ValidateHost(args[0]); err != nil {
				return usage("%v", err)
			}
			req.Host, req.Hub = args[0], h
			req.From, _ = os.Hostname()
			var facts *flows.ShareFacts
			err = o.withAuth(cmd, req.Host, func() error {
				if !o.json {
					fmt.Fprintf(cmd.ErrOrStderr(), "Looking at %s and the hub host…\n", req.Host)
				}
				var gerr error
				facts, gerr = flows.GatherShare(cmd.Context(), req, o.cfg.Targets)
				return gerr
			})
			if err != nil {
				return err
			}
			p, err := flows.BuildHubShare(req, facts, o.cfg.Targets)
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
				fmt.Fprintf(cmd.OutOrStdout(), "\nHub %s is registered on %s. There: `lazylongarm` (Hub tab), `lazylongarm hub status`, `lazylongarm hub panel`.\n", h.ID, req.Host)
			}
			return nil
		},
	}
	c.Flags().StringVar(&hubID, "hub", "", "hub id (default: the default hub)")
	c.Flags().BoolVar(&req.Targets, "targets", false, "also register the machines lazylongarm manages for this hub (aliases the machine resolves differently are skipped)")
	c.Flags().BoolVar(&req.NoAuthorize, "no-authorize", false, "do not add the machine's public key to the hub host's authorized_keys")
	c.Flags().StringVar(&req.Identity, "identity", "", "private key on the machine to reach the hub host with (e.g. ~/.ssh/work_ed25519): writes a Host alias lazylongarm-hub-<id> into a file its ~/.ssh/config already Includes")
	c.Flags().StringVar(&req.RemoteBin, "remote-lazylongarm", "", "lazylongarm binary on the machine (default: found on its PATH)")
	_ = c.Flags().MarkHidden("remote-lazylongarm")
	ao.bind(c)
	return c
}

// ---- hub import ----

func hubImportCommand(o *options) *cobra.Command {
	var stdin bool
	var d hubshare.Derive
	var ao applyOptions
	c := &cobra.Command{
		Use:   "import",
		Short: "Register a hub here: from `hub share` (--stdin) or from this machine's longarm",
		Long: `Register a hub in this machine's lazylongarm config without setting it up.

With --stdin it reads what ` + "`lazylongarm hub share`" + ` sends (that command runs it
for you over ssh). Without, it derives the hub from this machine's own
longarm client: the address in its hub.toml and the fingerprint of its CA;
give --ssh-host to manage the hub host from here too (status, panel, panel
logins, backup).

A hub with the same address and CA is updated in place, keeping this
machine's own settings (config_dir); a different hub under the same id is
refused (--id).`,
		Example: `  lazylongarm hub import --ssh-host hub-box
  lazylongarm hub import --dry-run
  lazylongarm hub share work-laptop        # on a machine that operates the hub: pushes it here`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.load(); err != nil {
				return err
			}
			var payload hubshare.Payload
			source := "longarm"
			if stdin {
				if cmd.Flags().Changed("ssh-host") || cmd.Flags().Changed("mode") || cmd.Flags().Changed("panel") || cmd.Flags().Changed("name") {
					return usage("--stdin takes the hub from the payload; only --id may rename it")
				}
				b, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
				if err != nil {
					return err
				}
				payload, err = hubshare.Decode(b)
				if err != nil {
					return usage("%v", err)
				}
				if d.ID != "" {
					payload.Hub.ID = d.ID
				}
				source = "stdin"
			} else {
				if !cmd.Flags().Changed("ssh-host") && o.interactive(cmd) {
					values, err := wizard.Edit(cmd.Context(), wizard.Spec{Title: "Import the hub from this machine's longarm", SubmitLabel: "Next", Fields: []wizard.Field{
						{Key: "ssh", Label: "Hub SSH host", Help: "optional: an alias or user@host of the hub machine, to manage it from here (status, panel, logins)"},
					}}, cmd.InOrStdin(), cmd.OutOrStdout())
					if err != nil {
						return err
					}
					d.SSHHost = strings.TrimSpace(values["ssh"])
				}
				if d.SSHHost != "" {
					if err := connection.ValidateHost(d.SSHHost); err != nil {
						return usage("--ssh-host: %v", err)
					}
				}
				if d.Mode != "" && d.Mode != config.ModePublic && d.Mode != config.ModePrivate {
					return usage("--mode must be public or private")
				}
				m, err := hubshare.LocalMembership(d.ConfigDir)
				if err != nil {
					return err
				}
				vctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
				if v, err := (&longarm.Client{ConfigDir: d.ConfigDir}).Version(vctx); err == nil {
					d.Version = v.Version
				}
				cancel()
				h, err := hubshare.DeriveHub(m, d)
				if err != nil {
					return usage("%v", err)
				}
				payload = hubshare.NewPayload(h, nil, "")
				payload.Hub.ConfigDir = d.ConfigDir
			}
			p, err := flows.BuildHubImport(flows.ImportRequest{Payload: payload, Source: source, Config: o.cfg, VerifySSH: !stdin})
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
			var merge hubshare.MergeResult
			_ = json.Unmarshal([]byte(env.Get("merge")), &merge)
			if o.json {
				_ = writeJSON(cmd.OutOrStdout(), map[string]any{"plan": p.Digest(), "results": results, "merge": merge})
			}
			if err != nil {
				return err
			}
			if !o.json {
				fmt.Fprintf(cmd.OutOrStdout(), "\nHub %s %s. Next: `lazylongarm hub status`, or `lazylongarm` (Hub tab).\n", merge.HubID, merge.Hub)
			}
			return nil
		},
	}
	fl := c.Flags()
	fl.BoolVar(&stdin, "stdin", false, "read the hub from `lazylongarm hub share` on stdin")
	fl.StringVar(&d.ID, "id", "", "hub id here (default: the shared id, or the address's first DNS label)")
	fl.StringVar(&d.Name, "name", "", "display name (default: the id)")
	fl.StringVar(&d.SSHHost, "ssh-host", "", "SSH host of the hub machine, to manage it from here")
	fl.StringVar(&d.Mode, "mode", "", "public | private (default: private for a tailnet/private IP address)")
	fl.StringVar(&d.Panel, "panel", "", "panel listen address on the hub host (default 127.0.0.1:7480)")
	fl.StringVar(&d.ConfigDir, "longarm-config-dir", "", "this machine's Longarm config dir (default: $LONGARM_CONFIG_DIR, else ~/.config/longarm)")
	_ = c.RegisterFlagCompletionFunc("ssh-host", completeSSHHost)
	ao.bind(c)
	return c
}
