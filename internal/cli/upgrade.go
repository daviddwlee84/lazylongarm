package cli

import (
	"fmt"

	"github.com/daviddwlee84/lazylongarm/internal/selfupdate"
	"github.com/daviddwlee84/lazylongarm/internal/wizard"
	"github.com/spf13/cobra"
)

func upgradeCommand(o *options) *cobra.Command {
	var check, yes bool
	c := &cobra.Command{
		Use:   "upgrade",
		Short: "Check for or install a newer lazylongarm release",
		Long: `Finds out who owns this executable and upgrades it the matching way:
Homebrew → brew upgrade; release archive → download the same tag's archive,
verify checksums.txt and replace this file; go install → build the tag with a
temporary GOBIN and replace this file. Local or modified builds are left alone.
--check only reports. Upgrading lazylongarm does not touch Longarm itself.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := selfupdate.Inspect(cmd.Context(), versionString())
			if err != nil && res.Owner == "" {
				return err
			}
			if check || o.json && !yes {
				if o.json {
					if err != nil {
						res.Notes = append(res.Notes, err.Error())
					}
					return writeJSON(cmd.OutOrStdout(), res)
				}
				printCheck(cmd, res)
				return err
			}
			if err != nil {
				return err
			}
			if res.Owner != selfupdate.OwnerHomebrew && res.UpToDate {
				fmt.Fprintf(cmd.OutOrStdout(), "lazylongarm %s is up to date\n", res.Current)
				return nil
			}
			if !res.Supported {
				printCheck(cmd, res)
				return usage("this installation cannot upgrade itself: %s", res.Strategy)
			}
			if !yes {
				if !o.interactive(cmd) {
					return usage("upgrade needs --yes when not in a terminal (inspect with --check)")
				}
				ok, err := wizard.Confirm(cmd.Context(), "Upgrade lazylongarm", fmt.Sprintf("%s → %s\n\n%s", res.Current, orDefault(res.Latest, "(Homebrew decides)"), res.Strategy), cmd.InOrStdin(), cmd.OutOrStdout())
				if err != nil || !ok {
					return wizard.ErrCanceled
				}
			}
			if err := selfupdate.Apply(cmd.Context(), res, cmd.ErrOrStderr()); err != nil {
				return err
			}
			if o.json {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"upgraded": true, "from": res.Current, "to": res.Latest, "owner": res.Owner})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "lazylongarm upgraded (%s)\n", res.Owner)
			return nil
		},
	}
	c.Flags().BoolVar(&check, "check", false, "only report the installed and latest versions and the upgrade method")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "upgrade without asking")
	return c
}

func printCheck(cmd *cobra.Command, r selfupdate.Check) {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "installed  %s\n", r.Current)
	if r.Latest != "" {
		state := "update available"
		if r.UpToDate {
			state = "up to date"
		}
		fmt.Fprintf(w, "latest     %s (%s)\n", r.Latest, state)
	}
	fmt.Fprintf(w, "executable %s\n", r.Resolved)
	fmt.Fprintf(w, "owner      %s\n", r.Owner)
	fmt.Fprintf(w, "method     %s\n", r.Strategy)
	for _, n := range r.Notes {
		fmt.Fprintf(w, "note       %s\n", n)
	}
}
