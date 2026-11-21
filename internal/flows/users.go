package flows

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/longarm"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/secret"
)

// Users on a hub (upstream multi-tenancy). The first `longarm admin user add`
// separates users at once: fleet workers (no owner) stay usable by every
// user, certificates without a user record act as users named after
// themselves, and admin certificates get no rights on other users' owned
// workers. So the operator moves to a user-bound admin certificate (user
// self) and makes its workers owned (worker add --reenroll --owner) before
// inviting anyone (user invite).

// ErrAlreadySelf: this machine's longarm already acts as the user, as admin.
var ErrAlreadySelf = errors.New("already done")

// SelfRequest describes `user self`: create the operator's Longarm user and
// switch this machine's longarm to an admin certificate of that user.
type SelfRequest struct {
	Hub       *config.Hub
	Name      string // Longarm user
	Device    string // node name of this machine's new certificate ("" = upstream's default)
	Panel     bool   // also create a web panel login (approves `longarm login` of later devices at /link)
	ConfigDir string // this machine's client config directory (backed up first)
}

// FleetWorkers returns the approved workers without an owner: every user of
// the hub may use them once users are separated.
func FleetWorkers(nodes []longarm.Node) []string {
	var out []string
	for _, n := range nodes {
		if n.IsWorker() && n.Owner == "" && n.State != "revoked" && n.State != "pending" {
			out = append(out, n.Name)
		}
	}
	sort.Strings(out)
	return out
}

// BuildUserSelf assembles the plan. who is this machine's current identity,
// users the hub's user list.
func BuildUserSelf(req SelfRequest, who longarm.WhoAmI, users longarm.UserList, client LongarmClient) (*plan.Plan, error) {
	if req.Hub == nil {
		return nil, errors.New("no hub configured: run `lazylongarm hub setup` first")
	}
	if !userNameRE.MatchString(req.Name) {
		return nil, fmt.Errorf("user name %q: lowercase letter first, then a-z 0-9 _ -, at most 17 characters", req.Name)
	}
	if req.Device != "" && !workerNameRE.MatchString(req.Device) {
		return nil, fmt.Errorf("device name %q must be letters, digits, '.', '_' or '-'", req.Device)
	}
	if who.User == req.Name && who.Role == "admin" {
		return nil, ErrAlreadySelf
	}
	if who.Role != "admin" {
		return nil, fmt.Errorf("this machine's longarm is %q (role %s): adding users needs an admin certificate", who.Name, dash(who.Role))
	}
	existing, exists := users.Find(req.Name)
	if exists && existing.DeviceCount > 0 {
		return nil, fmt.Errorf("user %s already has %d device(s), and the hub issues user-bound codes for a user's first device only: %s", req.Name, existing.DeviceCount, LaterDeviceHint(req.Name, existing.Panel))
	}
	device := req.Device
	if device == "" {
		device = req.Name
	}
	backup := filepath.Join(req.ConfigDir, "tls.bak-"+time.Now().UTC().Format("20060102T150405Z"))

	p := &plan.Plan{
		Kind:  "user-self",
		Key:   req.Hub.ID + "--" + req.Name,
		Title: fmt.Sprintf("Make this machine's longarm an admin of Longarm user %q", req.Name),
		Summary: [][2]string{
			{"hub", req.Hub.Address + " (" + req.Hub.ID + ")"},
			{"user", req.Name + map[bool]string{true: " (exists, no device yet)", false: " (new)"}[exists]},
			{"this machine", fmt.Sprintf("%s (role %s, user %s) → %s (admin, user %s)", dash(who.Name), dash(who.Role), dash(who.User), device, req.Name)},
			{"client config", req.ConfigDir + " (tls/ backed up to " + filepath.Base(backup) + ")"},
		},
	}
	if req.Panel && !exists {
		p.Summary = append(p.Summary, [2]string{"panel login", req.Name + ": generated password + TOTP, shown once after apply (approves `longarm login` of your later devices at /link)"})
	}
	if !(req.Panel && !exists) && !existing.Panel {
		p.Warnings = append(p.Warnings, fmt.Sprintf("no panel login for %s, and none can be added once it has a device (the hub refuses an admin's panel login for a user with machines): a later device's `longarm login` must then be approved at /link by an admin panel login named %s (`lazylongarm hub panel-user %s`), and gets an admin certificate. Keep --panel unless that is fine", req.Name, req.Name, req.Name))
	}
	if !users.Tenancy {
		p.Warnings = append(p.Warnings, "this is the hub's first user: it separates users from now on. Fleet workers stay usable by every user; make yours owned (`lazylongarm worker add <host> --reenroll --owner "+req.Name+"`) before inviting anyone")
	}
	if who.User != "" && who.User != req.Name {
		p.Warnings = append(p.Warnings, fmt.Sprintf("the old certificate %s (user %s) stays valid until you remove it (`lazylongarm user remove %s`, without --purge): until then it acts as its own user and sees only fleet workers", dash(who.Name), who.User, who.User))
	}
	p.Warnings = append(p.Warnings, "other programs that already loaded the old certificate (`longarm mcp` in agent sessions, the tray) keep using it: restart them afterwards")

	// 1. backup
	p.Steps = append(p.Steps, plan.Step{
		ID: "backup", Title: "Back up this machine's client certificate", Idempotent: true,
		Actions: []plan.Action{{Where: "local", Display: "copy " + filepath.Join(req.ConfigDir, "tls") + " → " + backup}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			src := filepath.Join(req.ConfigDir, "tls")
			if _, err := os.Stat(src); err != nil {
				env.Logf("  no %s to back up", src)
				return nil
			}
			if err := os.CopyFS(backup, os.DirFS(src)); err != nil {
				return err
			}
			if err := os.Chmod(backup, 0o700); err != nil {
				return err
			}
			env.Set("backup", backup)
			env.Logf("  backed up to %s", backup)
			return nil
		},
	})

	// 2. user + code
	mint := fmt.Sprintf("longarm admin user add %s --admin --device %s --ttl 15m", req.Name, device)
	if req.Panel && !exists {
		mint += " --panel"
	}
	if exists {
		mint = fmt.Sprintf("longarm admin user code %s --admin --device %s --ttl 15m", req.Name, device)
	}
	p.Steps = append(p.Steps, plan.Step{
		ID: "user", Title: map[bool]string{true: "Mint an admin join code for the user's first device", false: "Add the user and mint an admin join code for this machine"}[exists], Idempotent: false,
		Actions: []plan.Action{{Where: "local", Display: mint + "   → one-time code" + map[bool]string{true: ", panel password + TOTP", false: ""}[req.Panel && !exists]}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			r := longarm.UserAddRequest{Name: req.Name, Device: req.Device, Admin: true, Panel: req.Panel && !exists, TTL: 15 * time.Minute}
			var added longarm.UserAdded
			var err error
			if exists {
				added, err = client.AdminUserCode(ctx, r)
			} else {
				added, err = client.AdminUserAdd(ctx, r)
			}
			if err != nil {
				return err
			}
			env.SetSecret("user_code", added.Code)
			for k, s := range map[string]secret.Secret{"panel_password": added.Password, "panel_totp": added.TOTP, "panel_otpauth": added.Otpauth} {
				if !s.Empty() {
					env.SetSecret(k, s)
				}
			}
			env.Logf("  minted a one-time admin code for %s (valid 15 min)", device)
			return nil
		},
	})

	// 3. join
	p.Steps = append(p.Steps, plan.Step{
		ID: "join", Title: "Enroll this machine's longarm with the new certificate", Idempotent: false,
		Actions: []plan.Action{
			{Where: "local", Display: "longarm daemon stop"},
			{Where: "local", Display: fmt.Sprintf("longarm join %s <code> --ca-fingerprint %s --force", req.Hub.Address, shortFP(req.Hub.CAFingerprint))},
			{Where: "local", Display: "longarm daemon start"},
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			code := env.Secret("user_code")
			if code.Empty() {
				return errors.New("no join code from the previous step (rerun the plan)")
			}
			// The daemon keeps the identity it started with.
			if err := client.DaemonStop(ctx); err != nil {
				env.Logf("  ! longarm daemon stop: %v", err)
			}
			out, err := client.JoinForce(ctx, req.Hub.Address, code, req.Hub.CAFingerprint)
			if err != nil {
				return &plan.UnknownError{Err: plan.WithFix(err, restoreHint(req.ConfigDir, backup))}
			}
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if line != "" {
					env.Logf("  │ %s", line)
				}
			}
			if err := client.DaemonStart(ctx); err != nil {
				env.Logf("  ! longarm daemon start: %v", err)
			}
			return nil
		},
	})

	// 4. verify
	p.Steps = append(p.Steps, plan.Step{
		ID: "verify", Title: "Check the new identity", Idempotent: true, ReadOnly: true,
		Actions: []plan.Action{{Where: "local", Display: "longarm whoami --json   → user " + req.Name + ", role admin"}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			w, err := client.WhoAmI(ctx)
			if err != nil {
				return err
			}
			if w.User != req.Name || w.Role != "admin" {
				return plan.WithFix(fmt.Errorf("this machine is %s (role %s, user %s), not an admin of %s", w.Name, w.Role, w.User, req.Name), restoreHint(req.ConfigDir, backup))
			}
			env.Logf("  %s: admin, user %s", w.Name, w.User)
			return nil
		},
	})
	return p, nil
}

func restoreHint(configDir, backup string) string {
	return fmt.Sprintf("to go back: rm -rf %s && cp -Rp %s %s", filepath.Join(configDir, "tls"), backup, filepath.Join(configDir, "tls"))
}

// Invite is what a new user needs to join the hub and enroll her own
// machines without the operator's help (and without the web panel).
type Invite struct {
	User        string
	HubAddress  string
	Fingerprint string
	Code        secret.Secret
	Expires     time.Time
	Version     string            // release tag of the binaries to use
	Repo        string            // upstream repository (owner/name)
	Checksums   map[string]string // asset → sha256 (empty when unavailable)
	Password    secret.Secret     // panel login, when one was made
	TOTP        secret.Secret
}

// InvitePlatforms are the release platforms an invite lists.
var InvitePlatforms = []string{"darwin-arm64", "darwin-amd64", "linux-amd64", "linux-arm64"}

// RenderInvite writes the invite as Markdown. It holds a one-time code
// (and maybe a panel password): it is meant for one person.
func RenderInvite(inv Invite) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	w("# Longarm invite for %s", inv.User)
	w("")
	w("You have a Longarm user `%s` on the hub `%s`. These steps make your computer a client", inv.User, inv.HubAddress)
	w("(it can run jobs on your machines) and enroll the machines that should run jobs.")
	w("Your machines are yours only: other users of the hub can neither see nor use them unless you share them.")
	w("")
	w("**Keep this file to yourself.** The join code below works once and expires %s.", inv.Expires.Local().Format("2006-01-02 15:04 MST"))
	w("")
	w("## 1. Install longarm and longarmd (every machine)")
	w("")
	w("```sh")
	w("V=%s", inv.Version)
	w("P=darwin-arm64   # or darwin-amd64, linux-amd64, linux-arm64 (uname -sm)")
	w("mkdir -p ~/.local/bin && cd ~/.local/bin")
	w("curl -fLo longarm  https://github.com/%s/releases/download/$V/longarm-$P", inv.Repo)
	w("curl -fLo longarmd https://github.com/%s/releases/download/$V/longarmd-$P", inv.Repo)
	w("shasum -a 256 longarm longarmd   # compare with the table below")
	w("chmod +x longarm longarmd")
	w("```")
	w("")
	if len(inv.Checksums) > 0 {
		w("| file | sha256 |")
		w("|---|---|")
		for _, p := range InvitePlatforms {
			for _, c := range []string{"longarm", "longarmd"} {
				if sum := inv.Checksums[c+"-"+p]; sum != "" {
					w("| `%s-%s` | `%s` |", c, p, sum)
				}
			}
		}
	} else {
		w("Compare the sums with `SHA256SUMS` on https://github.com/%s/releases/tag/%s.", inv.Repo, inv.Version)
	}
	w("")
	w("## 2. Make your computer a client (once)")
	w("")
	w("```sh")
	w("~/.local/bin/longarm join %s %s --ca-fingerprint %s", inv.HubAddress, inv.Code.Reveal(), inv.Fingerprint)
	w("~/.local/bin/longarm whoami   # user %s", inv.User)
	w("```")
	w("")
	w("## 3. Enroll each machine that should run jobs")
	w("")
	w("On your client, mint a one-time worker code (valid 15 minutes, owned by you, no approval needed):")
	w("")
	w("```sh")
	w("~/.local/bin/longarm worker add <machine-name>")
	w("```")
	w("")
	w("On that machine (after step 1):")
	w("")
	w("```sh")
	w("~/.local/bin/longarmd join %s <worker-code> --ca-fingerprint %s --name <machine-name>", inv.HubAddress, inv.Fingerprint)
	w("sudo ~/.local/bin/longarmd service install --user \"$USER\" && sudo ~/.local/bin/longarmd service start")
	w("```")
	w("")
	w("Then `longarm ls` lists it; try `longarm exec <machine-name> -- uname -a`.")
	w("")
	w("## 4. Privacy: what the hub operator can see")
	w("")
	w("The hub's operator runs its certificate authority. By default the hub sees your jobs' command lines,")
	w("working directories and timings (never their output or files). To keep that to your machines, put this")
	w("into `~/.longarm/longarmd.toml` on each of them and restart the service (`longarmd service stop`, `start`):")
	w("")
	w("```toml")
	w("[access]")
	w("owners = [%q]", inv.User)
	w("users  = [%q]", inv.User)
	w("hub_job_details = false")
	w("```")
	w("")
	w("For protection even against a misused hub CA, add end-to-end trust: `longarm trust show` on your client,")
	w("then `longarmd trust add-user <that key line>` on each machine.")
	if !inv.Password.Empty() {
		w("")
		w("## Web panel login (optional)")
		w("")
		w("User `%s`, password `%s`, TOTP secret `%s` (add it to an authenticator app).", inv.User, inv.Password.Reveal(), inv.TOTP.Reveal())
	}
	return b.String()
}

// LaterDeviceHint says how a machine becomes another device of user, whose
// first device exists: upstream issues user-bound codes for the first device
// only, and refuses an admin's panel login for a user with machines.
func LaterDeviceHint(user string, hasPanel bool) string {
	approve := fmt.Sprintf("an admin panel login named %s (`lazylongarm hub panel-user %s`, over SSH on the hub host; the approved device gets an admin certificate)", user, user)
	if hasPanel {
		approve = user + "'s panel login (a client certificate), or " + approve
	}
	return fmt.Sprintf("run `longarm login --no-browser` on that machine and approve its code at the panel's /link page (`lazylongarm hub panel`; the URL longarm login prints is wrong when the panel listens on loopback only) with %s; or issue a client certificate on the hub host (`longarm admin issue --role client --user %s`)", approve, user)
}
