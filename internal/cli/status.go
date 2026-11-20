package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/longarm"
	"github.com/spf13/cobra"
)

type statusReport struct {
	Config    string                `json:"config"`
	Hubs      []config.Hub          `json:"hubs"`
	Targets   int                   `json:"targets"`
	Longarm   string                `json:"longarm,omitempty"`
	LongarmOK bool                  `json:"longarm_installed"`
	WhoAmI    *longarm.WhoAmI       `json:"whoami,omitempty"`
	Doctor    []longarm.DoctorCheck `json:"doctor,omitempty"`
	Errors    []string              `json:"errors,omitempty"`
}

func statusCommand(o *options) *cobra.Command {
	var doctor bool
	c := &cobra.Command{
		Use: "status", Short: "Show configured hubs and this machine's Longarm client state", Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.load(); err != nil {
				return err
			}
			rep := statusReport{Config: o.cfg.Path(), Hubs: o.cfg.Hubs, Targets: len(o.cfg.Targets)}
			ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
			defer cancel()
			hub, _ := o.cfg.Hub("")
			client := &longarm.Client{}
			if hub != nil {
				client.ConfigDir = hub.ConfigDir
			}
			if v, err := client.Version(ctx); err == nil {
				rep.Longarm, rep.LongarmOK = v.Version, true
				if w, err := client.WhoAmI(ctx); err == nil {
					rep.WhoAmI = &w
				} else {
					rep.Errors = append(rep.Errors, "whoami: "+firstLine(err.Error()))
				}
				if doctor {
					if d, err := client.Doctor(ctx); err == nil {
						rep.Doctor = d.Checks
					} else {
						rep.Errors = append(rep.Errors, "doctor: "+firstLine(err.Error()))
					}
				}
			} else {
				rep.Errors = append(rep.Errors, firstLine(err.Error()))
			}
			if o.json {
				return writeJSON(cmd.OutOrStdout(), rep)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "config   %s\n", rep.Config)
			if len(rep.Hubs) == 0 {
				fmt.Fprintln(w, "hubs     none — run `lazylongarm hub setup`")
			}
			for _, h := range rep.Hubs {
				def := ""
				if h.ID == o.cfg.DefaultHub {
					def = " (default)"
				}
				fmt.Fprintf(w, "hub      %s%s  %s  %s  ssh %s\n", h.ID, def, h.Address, h.Mode, dash(h.SSHHost))
			}
			fmt.Fprintf(w, "targets  %d managed machine(s)\n", rep.Targets)
			if rep.LongarmOK {
				fmt.Fprintf(w, "longarm  %s\n", rep.Longarm)
			}
			if rep.WhoAmI != nil {
				fmt.Fprintf(w, "client   %s (%s) on hub %q, certificate until %s\n", rep.WhoAmI.Name, rep.WhoAmI.Role, rep.WhoAmI.HubName, rep.WhoAmI.CertNotAfter)
			}
			for _, d := range rep.Doctor {
				mark := map[string]string{"ok": "✓", "warn": "!", "fail": "✗", "skip": "-", "info": "i"}[d.Status]
				fmt.Fprintf(w, "  %s %-16s %s\n", dash(mark), d.Title, d.Detail)
				if d.Fix != "" && d.Status != "ok" {
					fmt.Fprintf(w, "      fix: %s\n", d.Fix)
				}
			}
			for _, e := range rep.Errors {
				fmt.Fprintf(w, "  ! %s\n", e)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&doctor, "doctor", false, "include `longarm doctor` checks")
	return c
}

func settingsCommand(o *options) *cobra.Command {
	c := &cobra.Command{Use: "settings", Short: "Show the effective configuration and its path", Args: cobra.ArbitraryArgs, RunE: groupHelp}
	c.AddCommand(&cobra.Command{
		Use: "path", Short: "Print the config file path", Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _, err := config.ResolvePath(o.configPath)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), path)
			return err
		},
	}, &cobra.Command{
		Use: "show", Short: "Print the effective configuration", Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.load(); err != nil {
				return err
			}
			state, _ := config.StateDir()
			cache, _ := config.CacheDir()
			return writeJSON(cmd.OutOrStdout(), map[string]any{"path": o.cfg.Path(), "state_dir": state, "cache_dir": cache, "config": o.cfg})
		},
	})
	return c
}
