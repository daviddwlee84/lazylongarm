package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/flows"
	"github.com/daviddwlee84/lazylongarm/internal/longarm"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/release"
	"github.com/daviddwlee84/lazylongarm/internal/wizard"
	"github.com/skip2/go-qrcode"
	"github.com/spf13/cobra"
)

func userCommand(o *options) *cobra.Command {
	c := &cobra.Command{
		Use:   "user",
		Short: "Longarm users on your hub: become one, invite someone, remove one",
		Long: `Several people can share a hub, each with their own machines. The hub
separates users as soon as the first one exists, so the order matters:

  1. lazylongarm user self <you>                          your admin certificate gets a user
  2. lazylongarm worker add <host> --reenroll --owner <you>   for each fleet worker (every user may use those)
  3. lazylongarm user invite <name>                       a file with everything the new user needs

Your owned workers stay yours: other users can neither see nor use them,
and admins are not exempt. Share one later with ` + "`longarm worker share`" + `.`,
		Args: cobra.ArbitraryArgs, RunE: groupHelp,
	}
	c.AddCommand(userListCommand(o), userSelfCommand(o), userInviteCommand(o), userRemoveCommand(o))
	return c
}

func (o *options) hubClient(hubID string) (*config.Hub, *longarm.Client, error) {
	h, err := o.requireHub(hubID)
	if err != nil {
		return nil, nil, err
	}
	return h, &longarm.Client{ConfigDir: h.ConfigDir}, nil
}

// ---- user ls ----

func userListCommand(o *options) *cobra.Command {
	var hubID string
	c := &cobra.Command{
		Use: "ls", Short: "List the hub's users and the fleet workers every user may use", Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, client, err := o.hubClient(hubID)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			users, err := client.AdminUsers(ctx)
			if err != nil {
				return err
			}
			list, lerr := client.Workers(ctx)
			fleetWorkers := flows.FleetWorkers(list.Nodes)
			if o.json {
				out := map[string]any{"tenancy": users.Tenancy, "users": users.Users, "fleet_workers": fleetWorkers}
				if lerr != nil {
					out["hub_error"] = lerr.Error()
				}
				return writeJSON(cmd.OutOrStdout(), out)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "users separated: %s\n", map[bool]string{true: "yes", false: "no (no users yet)"}[users.Tenancy])
			if len(users.Users) > 0 {
				tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "USER\tSTATE\tDEVICES\tWORKERS\tPANEL\tCAN SHARE\tTEAMS")
				for _, u := range users.Users {
					fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%v\t%v\t%s\n", u.Name, map[bool]string{true: "disabled", false: "active"}[u.Disabled], u.DeviceCount, u.WorkerCount, u.Panel, u.CanShare, dash(strings.Join(u.Teams, ",")))
				}
				if err := tw.Flush(); err != nil {
					return err
				}
			}
			switch {
			case lerr != nil:
				fmt.Fprintf(cmd.ErrOrStderr(), "! worker list unavailable: %v\n", lerr)
			case len(fleetWorkers) > 0:
				fmt.Fprintf(w, "fleet workers (every user may use them): %s\n", strings.Join(fleetWorkers, ", "))
				fmt.Fprintln(w, "  make yours owned: lazylongarm worker add <host> --reenroll --owner <you>")
			}
			return nil
		},
	}
	c.Flags().StringVar(&hubID, "hub", "", "hub id (default: the default hub)")
	return c
}

// ---- user self ----

func userSelfCommand(o *options) *cobra.Command {
	var hubID string
	var req flows.SelfRequest
	var ao applyOptions
	c := &cobra.Command{
		Use:   "self <user>",
		Short: "Create your Longarm user and switch this machine to an admin certificate of it",
		Long: `Adds the Longarm user (the hub's first user separates users), mints an
admin join code for this machine and enrolls this machine's longarm with it
(the old certificate is backed up first). With --panel (default) it also
creates a web panel login, shown once, which approves ` + "`longarm login`" + ` of
your later devices at the panel's /link page. This is the only chance: the
hub refuses an admin's panel login for a user who has machines. Without one,
a later device is approved by an admin panel login named like the user
(` + "`lazylongarm hub panel-user <user>`" + `) and gets an admin certificate.

Afterwards make your workers owned (` + "`lazylongarm worker add <host> --reenroll --owner <user>`" + `)
and remove the old certificate's user (` + "`lazylongarm user remove <old>`" + `, without --purge).`,
		Example: `  lazylongarm user self alice --device alice-laptop --dry-run
  lazylongarm user self alice --device alice-laptop`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			h, client, err := o.hubClient(hubID)
			if err != nil {
				return err
			}
			req.Hub, req.Name = h, args[0]
			req.ConfigDir = h.ConfigDir
			if req.ConfigDir == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				req.ConfigDir = filepath.Join(home, ".config", "longarm")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			who, werr := client.WhoAmI(ctx)
			users, uerr := client.AdminUsers(ctx)
			cancel()
			if werr != nil {
				return fmt.Errorf("this machine's longarm: %w", werr)
			}
			if uerr != nil {
				return fmt.Errorf("list users: %w", uerr)
			}
			p, err := flows.BuildUserSelf(req, who, users, client)
			if errors.Is(err, flows.ErrAlreadySelf) {
				if o.json {
					return writeJSON(cmd.OutOrStdout(), map[string]any{"done": true, "user": who.User, "name": who.Name})
				}
				fmt.Fprintf(cmd.OutOrStdout(), "This machine's longarm (%s) is already an admin of user %s.\n", who.Name, who.User)
				return nil
			}
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
			out := cmd.OutOrStdout()
			if pw := env.Secret("panel_password"); !pw.Empty() && !o.json {
				fmt.Fprintf(out, "\nWeb panel login %s (shown once):\n  password     %s\n  TOTP secret  %s\n\n", req.Name, pw.Reveal(), env.Secret("panel_totp").Reveal())
				if uri := env.Secret("panel_otpauth").Reveal(); uri != "" {
					if code, err := qrcode.New(uri, qrcode.Medium); err == nil {
						fmt.Fprint(out, code.ToSmallString(false))
					}
				}
				fmt.Fprintln(out, "→ Scan the QR code with your authenticator app now and keep the password in your password manager.")
				fmt.Fprintln(out, "  It approves `longarm login` of your later devices at the panel's /link page.")
			}
			if !o.json {
				fmt.Fprintf(out, "\nThis machine's longarm is now an admin of user %s.\n\nNext:\n", req.Name)
				fmt.Fprintf(out, "  lazylongarm user ls                                        # fleet workers every user may use\n")
				fmt.Fprintf(out, "  lazylongarm worker add <host> --reenroll --owner %s     # for each of them\n", req.Name)
				if who.User != "" && who.User != req.Name {
					fmt.Fprintf(out, "  lazylongarm user remove %s                               # the old certificate (no --purge)\n", who.User)
				}
				fmt.Fprintln(out, "  restart agent sessions that run `longarm mcp` (they hold the old certificate)")
			}
			return nil
		},
	}
	c.Flags().StringVar(&hubID, "hub", "", "hub id (default: the default hub)")
	c.Flags().StringVar(&req.Device, "device", "", "node name of this machine's new certificate (default: the user name)")
	c.Flags().BoolVar(&req.Panel, "panel", true, "also create a web panel login (approves your later devices at /link; it cannot be added later)")
	ao.bind(c)
	return c
}

// ---- user invite ----

func userInviteCommand(o *options) *cobra.Command {
	var hubID, outPath string
	var ttl time.Duration
	var panel, allowFleet, yes bool
	var maxJobs int
	c := &cobra.Command{
		Use:   "invite <user>",
		Short: "Add a Longarm user and write an invite: install, join and worker steps for that person",
		Long: `Adds the user with a one-time client join code and writes an invite (Markdown)
with everything that person needs: the binaries to download and their
sha256, the join command, how to enroll their own machines (owned by them,
no approval) and what the hub's operator can see. Nobody needs SSH access to
anybody else's machine, and the web panel is not needed.

It refuses while the hub has fleet workers (no owner): every user may use
those. Make yours owned first, or pass --allow-fleet-workers.`,
		Example: `  lazylongarm user invite alice --out alice-invite.md
  lazylongarm user invite alice --ttl 72h --max-jobs 8 --out alice-invite.md`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			h, client, err := o.hubClient(hubID)
			if err != nil {
				return err
			}
			name := args[0]
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			users, err := client.AdminUsers(ctx)
			if err != nil {
				return err
			}
			if _, ok := users.Find(name); ok {
				return usage("user %s already exists (a new code for a user without devices: longarm admin user code %s)", name, name)
			}
			list, err := client.Workers(ctx)
			if err != nil {
				return fmt.Errorf("list workers: %w", err)
			}
			if fw := flows.FleetWorkers(list.Nodes); len(fw) > 0 && !allowFleet {
				return usage("%s would be able to use these fleet workers: %s\nmake them owned first (lazylongarm worker add <host> --reenroll --owner <you>), or pass --allow-fleet-workers", name, strings.Join(fw, ", "))
			}
			if outPath == "" && !o.interactive(cmd) && !yes {
				return usage("the invite holds a one-time code: write it to a file with --out, or pass --yes to print it")
			}
			if !yes && o.interactive(cmd) {
				body := fmt.Sprintf("Add user %s on %s and mint a client join code valid %s.\n\n", name, h.Address, ttl)
				if !users.Tenancy {
					body += "This is the hub's first user: it separates users from now on.\n"
				}
				body += fmt.Sprintf("The invite goes to %s.", map[bool]string{true: outPath + " (mode 0600)", false: "this terminal"}[outPath != ""])
				ok, err := wizard.Confirm(cmd.Context(), "Invite "+name, body, cmd.InOrStdin(), cmd.OutOrStdout())
				if err != nil {
					return err
				}
				if !ok {
					return wizard.ErrCanceled
				}
			}
			var quota []string
			if maxJobs > 0 {
				quota = append(quota, fmt.Sprintf("--max-jobs=%d", maxJobs))
			}
			added, err := client.AdminUserAdd(ctx, longarm.UserAddRequest{Name: name, Panel: panel, TTL: ttl, Quota: quota})
			if err != nil {
				return err
			}
			tag := h.Version
			if tag == "" {
				if v, err := client.Version(ctx); err == nil && v.Version != "" {
					tag = "v" + strings.TrimPrefix(v.Version, "v")
				}
			}
			inv := flows.Invite{User: name, HubAddress: h.Address, Fingerprint: h.CAFingerprint, Code: added.Code,
				Expires: time.Now().Add(ttl), Version: tag, Repo: release.Repo, Password: added.Password, TOTP: added.TOTP,
				Checksums: inviteChecksums(cmd.Context(), tag)}
			text := flows.RenderInvite(inv)
			if outPath == "" {
				fmt.Fprint(cmd.OutOrStdout(), text)
				return nil
			}
			if err := os.WriteFile(outPath, []byte(text), 0o600); err != nil {
				return fmt.Errorf("write the invite (user %s exists now; a new code: longarm admin user code %s): %w", name, name, err)
			}
			if o.json {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"user": name, "invite": outPath, "expires": inv.Expires})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Added user %s. Invite written to %s (mode 0600); the code expires %s.\n", name, outPath, inv.Expires.Local().Format("01-02 15:04"))
			fmt.Fprintln(cmd.OutOrStdout(), "Send it over a private channel. Their machines will be theirs only; `lazylongarm user ls` shows counts.")
			return nil
		},
	}
	c.Flags().StringVar(&hubID, "hub", "", "hub id (default: the default hub)")
	c.Flags().StringVarP(&outPath, "out", "o", "", "write the invite to this file (mode 0600) instead of the terminal")
	c.Flags().DurationVar(&ttl, "ttl", 24*time.Hour, "validity of the one-time join code")
	c.Flags().BoolVar(&panel, "panel", false, "also create a web panel login for them (useful only if they can reach the panel)")
	c.Flags().IntVar(&maxJobs, "max-jobs", 0, "quota: their active jobs in the whole fleet (0 = no limit)")
	c.Flags().BoolVar(&allowFleet, "allow-fleet-workers", false, "invite although fleet workers exist (they may use them)")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "no confirmation (required when not in a terminal)")
	return c
}

// releaseChecksum is release.Checksum (replaced in tests).
var releaseChecksum = release.Checksum

// inviteChecksums looks up the sha256 of every binary an invite lists;
// nil when the release's SHA256SUMS is unavailable.
func inviteChecksums(ctx context.Context, tag string) map[string]string {
	cacheDir, err := config.CacheDir()
	if err != nil || tag == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out := map[string]string{}
	for _, p := range flows.InvitePlatforms {
		goos, goarch, _ := strings.Cut(p, "-")
		for _, c := range []string{"longarm", "longarmd"} {
			asset := release.AssetName(c, goos, goarch)
			sum, err := releaseChecksum(ctx, cacheDir, tag, asset)
			if err != nil {
				return nil
			}
			out[asset] = sum
		}
	}
	return out
}

// ---- user remove ----

func userRemoveCommand(o *options) *cobra.Command {
	var hubID string
	var purge, yes bool
	c := &cobra.Command{
		Use:   "remove <user>",
		Short: "Remove a user: revoke all its devices and workers (--purge also deletes its jobs and data on workers)",
		Long: `Revokes the certificates of all the user's devices and owned workers and
deletes the user and its panel login. Also works for a certificate without
a user record (it acts as a user named after itself), e.g. the old admin
certificate after ` + "`lazylongarm user self`" + `.

--purge also makes every worker kill the user's jobs and sessions and delete
their records (and, with per-user isolation, the user's data and OS account).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, client, err := o.hubClient(hubID)
			if err != nil {
				return err
			}
			name := args[0]
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			who, err := client.WhoAmI(ctx)
			if err != nil {
				return err
			}
			if who.User == name {
				return usage("this machine's longarm acts as user %s: removing it would revoke this machine too", name)
			}
			if !yes {
				if !o.interactive(cmd) {
					return usage("removing user %s needs confirmation: pass --yes", name)
				}
				body := fmt.Sprintf("Revoke every device and owned worker of user %s and delete the user.", name)
				if purge {
					body += "\n\n--purge: workers also kill the user's jobs and sessions and delete their records (and, with per-user isolation, its data and OS account). This cannot be undone."
				}
				ok, err := wizard.Confirm(cmd.Context(), "Remove user "+name, body, cmd.InOrStdin(), cmd.OutOrStdout())
				if err != nil {
					return err
				}
				if !ok {
					return wizard.ErrCanceled
				}
			}
			out, err := client.AdminUserRemove(ctx, name, purge)
			if err != nil {
				return err
			}
			if o.json {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"removed": name, "purge": purge, "output": strings.TrimSpace(string(out))})
			}
			fmt.Fprint(cmd.OutOrStdout(), string(out))
			return nil
		},
	}
	c.Flags().StringVar(&hubID, "hub", "", "hub id (default: the default hub)")
	c.Flags().BoolVar(&purge, "purge", false, "also delete the user's jobs, data and OS account on every worker")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "no confirmation (required when not in a terminal)")
	return c
}
