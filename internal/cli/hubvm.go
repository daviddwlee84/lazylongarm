package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/cloudvm"
	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/flows"
	"github.com/daviddwlee84/lazylongarm/internal/longarm"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/wizard"
	"github.com/spf13/cobra"
)

func hubVMCommand(o *options) *cobra.Command {
	c := &cobra.Command{Use: "vm", Short: "Manage the hub's cloud VM (Azure): status, start, stop, restart, delete", Args: cobra.ArbitraryArgs, RunE: groupHelp}
	c.AddCommand(hubVMStatusCommand(o),
		hubVMActionCommand(o, "start", "start", "Start the hub VM"),
		hubVMActionCommand(o, "stop", "deallocate", "Stop and deallocate the hub VM (compute billing stops)"),
		hubVMActionCommand(o, "restart", "restart", "Restart the hub VM"),
		hubVMDeleteCommand(o))
	return c
}

func (o *options) hubVM(hubID string) (*config.Hub, cloudvm.Ref, error) {
	h, err := o.requireHub(hubID)
	if err != nil {
		return nil, cloudvm.Ref{}, err
	}
	if h.Cloud == nil || h.Cloud.Provider != "azure" || h.Cloud.VM == "" {
		return nil, cloudvm.Ref{}, usage("hub %s has no recorded Azure VM (it is set when hub setup opens the Azure firewall)", h.ID)
	}
	return h, cloudvm.Ref{Subscription: h.Cloud.Subscription, ResourceGroup: h.Cloud.ResourceGroup, Name: h.Cloud.VM}, nil
}

type vmReport struct {
	Hub         string            `json:"hub"`
	VM          cloudvm.VM        `json:"vm"`
	EgressBytes int64             `json:"egress_bytes_month"`
	Estimate    *cloudvm.Estimate `json:"estimate,omitempty"`
	Errors      []string          `json:"errors,omitempty"`
}

func hubVMStatusCommand(o *options) *cobra.Command {
	var hubID string
	c := &cobra.Command{
		Use: "status", Short: "Show the hub VM's power state, size, traffic this month and a cost estimate", Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			h, ref, err := o.hubVM(hubID)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			vm, err := cloudvm.Show(ctx, ref)
			if err != nil {
				return err
			}
			rep := vmReport{Hub: h.ID, VM: vm}
			now := time.Now().UTC()
			monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
			if b, err := cloudvm.NetworkOut(ctx, ref.Subscription, vm.ID, monthStart); err == nil {
				rep.EgressBytes = b
			} else {
				rep.Errors = append(rep.Errors, "traffic: "+firstLine(err.Error()))
			}
			if e, err := cloudvm.EstimateCost(ctx, vm, rep.EgressBytes); err == nil {
				rep.Estimate = &e
			} else {
				rep.Errors = append(rep.Errors, "price: "+firstLine(err.Error()))
			}
			if o.json {
				return writeJSON(cmd.OutOrStdout(), rep)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "hub %s · VM %s (%s, %s)\n", h.ID, vm.Name, ref.ResourceGroup, vm.Location)
			fmt.Fprintf(w, "  power      %s\n", vm.Power)
			fmt.Fprintf(w, "  size       %s · disk %s %d GB\n", vm.Size, vm.DiskSKU, vm.DiskGB)
			fmt.Fprintf(w, "  address    %s · %s\n", dash(vm.FQDNs), dash(vm.PublicIPs))
			fmt.Fprintf(w, "  egress     %.2f GB this month (since %s)\n", float64(rep.EgressBytes)/1e9, monthStart.Format("2006-01-02"))
			if e := rep.Estimate; e != nil {
				fmt.Fprintf(w, "  estimate   ≈ US$%.2f/month: compute %.2f (%.4f/h) + IP %.2f + disk %.2f + egress %.2f\n", e.Total, e.ComputeMonth, e.ComputeHourly, e.IPMonth, e.DiskMonth, e.EgressMonth)
				for _, n := range e.Notes {
					fmt.Fprintf(w, "             %s\n", n)
				}
			}
			for _, e := range rep.Errors {
				fmt.Fprintf(w, "  ! %s\n", e)
			}
			return nil
		},
	}
	c.Flags().StringVar(&hubID, "hub", "", "hub id")
	return c
}

func hubVMActionCommand(o *options, use, verb, short string) *cobra.Command {
	var hubID string
	var ao applyOptions
	c := &cobra.Command{
		Use: use, Short: short, Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			h, ref, err := o.hubVM(hubID)
			if err != nil {
				return err
			}
			p := vmActionPlan(h, ref, use, verb)
			if ao.dryRun {
				return o.printPlan(cmd, p)
			}
			if err := o.review(cmd, p, ao); err != nil {
				if errors.Is(err, errBack) {
					return wizard.ErrCanceled
				}
				return err
			}
			_, err = o.apply(cmd, p, plan.NewEnv(), ao)
			return err
		},
	}
	c.Flags().StringVar(&hubID, "hub", "", "hub id")
	ao.bind(c)
	return c
}

func vmActionPlan(h *config.Hub, ref cloudvm.Ref, use, verb string) *plan.Plan {
	p := &plan.Plan{Kind: "hub-vm-" + use, Key: h.ID, Title: fmt.Sprintf("%s the hub VM %s (%s)", strings.ToUpper(use[:1])+use[1:], ref.Name, h.ID),
		Summary: [][2]string{{"vm", ref.Name + " · " + ref.ResourceGroup}, {"subscription", ref.Subscription}, {"hub", h.Address}}}
	switch use {
	case "stop":
		p.Warnings = append(p.Warnings,
			"the hub goes offline: workers and clients cannot reach it until `lazylongarm hub vm start`",
			"jobs already running on workers keep running; new jobs cannot be scheduled",
			"deallocate stops compute billing; the static public IP and the disk are still billed")
	case "restart":
		p.Warnings = append(p.Warnings, "the hub is offline for about a minute; running jobs keep running")
	}
	p.Steps = []plan.Step{{
		ID: use, Title: fmt.Sprintf("az vm %s %s", verb, ref.Name), Idempotent: true,
		Actions: []plan.Action{{Where: "azure", Display: fmt.Sprintf("az vm %s --subscription %s --resource-group %s --name %s", verb, ref.Subscription, ref.ResourceGroup, ref.Name)}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			vm, err := cloudvm.Show(ctx, ref)
			if err != nil {
				return false, err
			}
			env.Logf("  currently: %s", vm.Power)
			switch use {
			case "start":
				return vm.Running(), nil
			case "stop":
				return vm.Deallocated(), nil
			}
			return false, nil
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			env.Logf("  az vm %s (takes a minute or two)…", verb)
			if err := cloudvm.Action(ctx, ref, verb); err != nil {
				return &plan.UnknownError{Err: err}
			}
			return nil
		},
		Verify: func(ctx context.Context, env *plan.Env) error {
			vm, err := cloudvm.Show(ctx, ref)
			if err != nil {
				return err
			}
			env.Logf("  now: %s", vm.Power)
			if (use == "start" || use == "restart") && !vm.Running() {
				return fmt.Errorf("VM is %s, expected running", vm.Power)
			}
			if use == "stop" && !vm.Deallocated() {
				return fmt.Errorf("VM is %s, expected deallocated", vm.Power)
			}
			return nil
		},
	}}
	if use == "start" || use == "restart" {
		p.Steps = append(p.Steps, plan.Step{
			ID: "hub-reachable", Title: "Wait until the hub answers on " + h.Address, Idempotent: true, ReadOnly: true,
			Actions: []plan.Action{{Where: "local", Display: "longarm whoami (an mTLS round trip; a TCP connect can be faked by a proxy TUN), up to 3 min"}},
			Apply: func(ctx context.Context, env *plan.Env) error {
				deadline := time.Now().Add(3 * time.Minute)
				client := &longarm.Client{ConfigDir: h.ConfigDir}
				_, noClient := longarm.Find()
				for {
					var err error
					if noClient == nil {
						var w longarm.WhoAmI
						if w, err = client.WhoAmI(ctx); err == nil {
							env.Logf("  hub answered over mTLS (%s as %s)", w.HubName, w.Role)
							return nil
						}
					} else if err = dialTCP(ctx, h.Address); err == nil {
						env.Logf("  hub accepts TCP connections (longarm is not installed here; no mTLS check)")
						return nil
					}
					if time.Now().After(deadline) {
						return plan.WithFix(errors.New("the hub did not answer within 3 minutes"), "lazylongarm hub status")
					}
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(5 * time.Second):
					}
				}
			},
		})
	}
	return p
}

func hubVMDeleteCommand(o *options) *cobra.Command {
	var hubID string
	var backupConfirmed, keepConfig bool
	var ao applyOptions
	c := &cobra.Command{
		Use:   "delete",
		Short: "Delete the hub VM's resource group (only one tagged as lazylongarm's hub group)",
		Long: `Deletes the whole resource group of the hub VM - VM, disk, NIC, public IP,
NSG, VNet - and forgets the hub (and its workers) in lazylongarm's config.
Refused unless the group is tagged managed-by=lazylongarm,purpose=longarm-hub.
Requires a hub backup from the last 7 days in $XDG_STATE_HOME/lazylongarm/backups
(or --backup-confirmed when you keep it elsewhere, e.g. in a password manager).
Workers keep their certificates; remove them with 'longarm worker remove' first
or 'longarmd leave' on each machine.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			h, ref, err := o.hubVM(hubID)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			tags, err := cloudvm.RGTags(ctx, ref.Subscription, ref.ResourceGroup)
			if err != nil {
				return err
			}
			if !cloudvm.OwnedRG(tags) {
				return usage("resource group %s is not tagged managed-by=lazylongarm,purpose=longarm-hub; delete it yourself in the Azure portal or with az", ref.ResourceGroup)
			}
			backup, ok := recentBackup(h.ID)
			if !ok && !backupConfirmed {
				return &plan.FixError{Err: errors.New("no hub backup from the last 7 days in the backups directory"), Fix: "lazylongarm hub backup   (or --backup-confirmed if you keep one elsewhere)"}
			}
			p := vmDeletePlan(o, h, ref, backup, keepConfig)
			if ao.dryRun {
				return o.printPlan(cmd, p)
			}
			if err := o.review(cmd, p, ao); err != nil {
				if errors.Is(err, errBack) {
					return wizard.ErrCanceled
				}
				return err
			}
			if !ao.yes {
				values, err := wizard.Edit(ctx, wizard.Spec{Title: "Type the resource group name to delete it", Description: "This deletes " + ref.ResourceGroup + " and everything in it. It cannot be undone.", SubmitLabel: "Delete",
					Fields: []wizard.Field{{Key: "rg", Label: "Resource group", Required: true}}}, cmd.InOrStdin(), cmd.OutOrStdout())
				if err != nil {
					return err
				}
				if !strings.EqualFold(strings.TrimSpace(values["rg"]), ref.ResourceGroup) {
					return usage("the name did not match; nothing was deleted")
				}
			}
			_, err = o.apply(cmd, p, plan.NewEnv(), ao)
			return err
		},
	}
	c.Flags().StringVar(&hubID, "hub", "", "hub id")
	c.Flags().BoolVar(&backupConfirmed, "backup-confirmed", false, "I keep a recent hub backup outside the backups directory")
	c.Flags().BoolVar(&keepConfig, "keep-config", false, "keep the hub profile in lazylongarm's config")
	ao.bind(c)
	return c
}

func vmDeletePlan(o *options, h *config.Hub, ref cloudvm.Ref, backup string, keepConfig bool) *plan.Plan {
	p := &plan.Plan{Kind: "hub-vm-delete", Key: h.ID, Title: fmt.Sprintf("Delete hub %s: resource group %s", h.ID, ref.ResourceGroup),
		Summary: [][2]string{{"resource group", ref.ResourceGroup + " (tagged managed-by=lazylongarm)"}, {"subscription", ref.Subscription}, {"backup", orDefault(backup, "confirmed by you (--backup-confirmed)")}},
		Warnings: []string{
			"the hub's CA and database are deleted: every worker and client certificate of this hub becomes useless",
			"workers keep running longarmd against a hub that no longer exists: run `longarmd leave` (or reinstall) on each",
		}}
	p.Steps = []plan.Step{{
		ID: "delete-rg", Title: "Delete resource group " + ref.ResourceGroup, Idempotent: true,
		Actions: []plan.Action{{Where: "azure", Display: cloudvm.DeleteArgs(ref.Subscription, ref.ResourceGroup)}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			exists, err := cloudvm.RGExists(ctx, ref.Subscription, ref.ResourceGroup)
			return err == nil && !exists, err
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			env.Logf("  deleting (several minutes)…")
			if err := cloudvm.DeleteRG(ctx, ref.Subscription, ref.ResourceGroup); err != nil {
				return &plan.UnknownError{Err: err}
			}
			return nil
		},
	}}
	if !keepConfig {
		p.Steps = append(p.Steps, plan.Step{
			ID: "forget-hub", Title: "Forget hub " + h.ID + " and its targets in lazylongarm's config", Idempotent: true,
			Actions: []plan.Action{{Where: "local", Display: "remove [[hubs]] " + h.ID + " and targets with hub = " + h.ID}},
			Apply: func(ctx context.Context, env *plan.Env) error {
				id := h.ID // h may point into the config that Update replaces
				return o.cfg.Update(func(c *config.Config) error {
					c.RemoveHub(id)
					return nil
				})
			},
		})
	}
	return p
}

// recentBackup finds a backup of hub id from the last 7 days.
func recentBackup(id string) (string, bool) {
	state, err := config.StateDir()
	if err != nil {
		return "", false
	}
	matches, _ := filepath.Glob(filepath.Join(state, "backups", "longarm-hub-"+id+"-*"))
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && time.Since(st.ModTime()) < 7*24*time.Hour {
			return m, true
		}
	}
	return "", false
}

// dialTCP is swappable in tests.
var dialTCP = flows.DialTCP
