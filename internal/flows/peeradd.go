package flows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/hostprobe"
	"github.com/daviddwlee84/lazylongarm/internal/longarm"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
)

// PeerRequest describes `peer add`: a worker whose own longarm is also
// enrolled as a client, so sessions on it can dispatch to other workers.
type PeerRequest struct {
	WorkerRequest
	// ClientName is the client node's name. Node names are unique across
	// roles on the hub, so it cannot equal the worker name (default
	// "<worker>-client").
	ClientName string
	// ClientUser is the Longarm user the client certificate belongs to. It
	// is required once the hub separates users; "" pins the node's own name.
	ClientUser string
	// Tenancy reports whether the hub separates users (from whoami).
	Tenancy bool
}

// PeerDefaults fills the worker defaults and the client name.
func PeerDefaults(req PeerRequest, f *hostprobe.Facts) PeerRequest {
	req.WorkerRequest = WorkerDefaults(req.WorkerRequest, f)
	if req.ClientName == "" && req.Name != "" {
		req.ClientName = req.Name + "-client"
	}
	return req
}

// ValidatePeer checks the worker part and the client part.
func ValidatePeer(req PeerRequest, f *hostprobe.Facts) error {
	var problems []string
	if err := ValidateWorker(req.WorkerRequest, f); err != nil {
		problems = append(problems, err.Error())
	}
	if !workerNameRE.MatchString(req.ClientName) {
		problems = append(problems, fmt.Sprintf("client name %q must be letters, digits, '.', '_' or '-'", req.ClientName))
	} else if req.ClientName == req.Name {
		problems = append(problems, "the client name must differ from the worker name (node names are unique on the hub)")
	}
	if req.Mint != MintAdmin {
		problems = append(problems, "peer add mints a client join code, which needs an admin certificate; without one, run `longarm login` on that machine instead")
	}
	if req.Tenancy && req.ClientUser == "" {
		problems = append(problems, "the hub separates users: pass --client-user (the Longarm user the machine acts as)")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// BuildPeerAdd assembles the worker add plan plus the client enrollment.
func BuildPeerAdd(req PeerRequest, f *hostprobe.Facts, deps WorkerDeps) (*plan.Plan, error) {
	req = PeerDefaults(req, f)
	if err := ValidatePeer(req, f); err != nil {
		return nil, err
	}
	p, err := BuildWorkerAdd(req.WorkerRequest, f, deps)
	if err != nil {
		return nil, err
	}
	host := req.Host
	where := host
	if where == "" {
		where = "local"
	}
	hub := req.Hub
	longarmPath := filepath.Join(f.Home, ".local", "bin", "longarm")

	p.Kind = "peer-add"
	p.Title = fmt.Sprintf("Add %s as Longarm peer %q (worker + client %q)", f.Where(), req.Name, req.ClientName)
	client := req.ClientName
	if req.ClientUser != "" {
		client += " (user " + req.ClientUser + ")"
	}
	p.Summary = append(p.Summary, [2]string{"client", client + ": its longarm can dispatch to every worker you can use"})
	p.Warnings = append(p.Warnings, fmt.Sprintf("jobs on %s run as %s, which will hold a client certificate: any job there can dispatch to every worker you can use (revoke: longarm admin revoke %s)", req.Name, f.User, req.ClientName))
	if why := f.RootEquivalent(); len(why) > 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf("%s is root-equivalent on %s (%s): one compromised job there gets root and the whole fleet", f.User, f.Where(), strings.Join(why, ", ")))
	}

	run := func(script string, args ...string) connection.Request {
		return connection.Request{Host: host, Script: script, Args: args, Timeout: 30 * time.Second}
	}
	hubHost := strings.Split(hub.Address, ":")[0]

	// The worker plan ends with save-target; the client steps go before a
	// save-target that also records the client.
	p.Steps = p.Steps[:len(p.Steps)-1]

	force := ""
	if req.Reenroll {
		force = " --force"
	}
	mint := fmt.Sprintf("longarm admin join-code --role client --name %s --ttl 15m", req.ClientName)
	if req.ClientUser != "" {
		mint += " --user " + req.ClientUser
	}
	p.Steps = append(p.Steps, plan.Step{
		ID: "enroll-client", Title: "Enroll the machine's longarm as a client (one-time code via stdin)", Idempotent: false,
		Actions: []plan.Action{
			{Where: "local", Display: mint + "   → one-time code (no approval needed)"},
			{Where: where, Display: fmt.Sprintf("%s join %s <code from stdin> --ca-fingerprint %s --name %s%s", longarmPath, hub.Address, shortFP(hub.CAFingerprint), req.ClientName, force)},
			{Where: where, Display: longarmPath + " daemon start"},
		},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			if req.Reenroll {
				return false, nil
			}
			res, err := env.RunQuiet(ctx, run(`d="$HOME/.config/longarm"
if [ ! -f "$d/tls/node.crt" ]; then echo none; exit 0; fi
if grep -qF "$1" "$d/hub.toml" 2>/dev/null; then echo this; else echo other; fi`, hubHost))
			if err != nil {
				return false, err
			}
			switch strings.TrimSpace(string(res.Stdout)) {
			case "this":
				env.Logf("  %s's longarm is already a client of %s", f.Where(), hub.Address)
				return true, nil
			case "other":
				return false, plan.WithFix(fmt.Errorf("%s's longarm is enrolled with another hub (~/.config/longarm)", f.Where()),
					"rerun with --reenroll to replace it, or `longarm leave` on that machine first")
			}
			return false, nil
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			code, err := deps.Client.AdminJoinCode(ctx, longarm.JoinCodeRequest{Role: "client", Name: req.ClientName, User: req.ClientUser, TTL: 15 * time.Minute})
			if err != nil {
				return err
			}
			env.SetSecret("client_code", code)
			env.Logf("  minted a one-time client code (valid 15 min)")
			// longarm join has no stdin form: the code reaches its argv on the
			// machine, but never the ssh command line, the review or the logs.
			r := run(`set -eu
IFS= read -r code
if [ "$5" = force ]; then "$1" join "$2" "$code" --ca-fingerprint "$3" --name "$4" --force; else "$1" join "$2" "$code" --ca-fingerprint "$3" --name "$4"; fi
"$1" daemon start >/dev/null 2>&1 || echo "note: longarm daemon start failed (only port forwards need it)"`,
				longarmPath, hub.Address, hub.CAFingerprint, req.ClientName, map[bool]string{true: "force", false: "-"}[req.Reenroll])
			r.Stdin = []byte(code.Reveal() + "\n")
			r.Timeout = 90 * time.Second
			if _, err := env.Run(ctx, r); err != nil {
				return &plan.UnknownError{Err: err}
			}
			return nil
		},
		Verify: func(ctx context.Context, env *plan.Env) error {
			res, err := env.RunQuiet(ctx, run(`"$1" whoami --json`, longarmPath))
			if err != nil {
				return fmt.Errorf("longarm whoami on %s: %w", f.Where(), err)
			}
			var w longarm.WhoAmI
			if err := json.Unmarshal(res.Stdout, &w); err != nil {
				return fmt.Errorf("longarm whoami on %s: %w", f.Where(), err)
			}
			if w.Role != "client" && w.Role != "admin" {
				return fmt.Errorf("%s's longarm is enrolled as %q, not as a client", f.Where(), w.Role)
			}
			env.Logf("  %s is %s (%s, user %s) on hub %q", f.Where(), w.Name, w.Role, w.User, w.HubName)
			return nil
		},
	})

	p.Steps = append(p.Steps, plan.Step{
		ID: "try-peer", Title: "Try it: list the fleet from the new peer", Idempotent: true, ReadOnly: true,
		Actions: []plan.Action{{Where: where, Display: longarmPath + " ls"}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			_, err := env.Run(ctx, run(`"$1" ls`, longarmPath))
			return err
		},
	})

	p.Steps = append(p.Steps, plan.Step{
		ID: "save-target", Title: "Record the peer in lazylongarm's config", Idempotent: true,
		Actions: []plan.Action{{Where: "local", Display: "save target (ssh host, worker name, client name, install + service mode)"}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			if deps.Config == nil {
				return nil
			}
			t := workerTarget(req.WorkerRequest, f)
			t.ClientName = req.ClientName
			deps.Config.UpsertTarget(t)
			if err := deps.Config.Save(); err != nil {
				return err
			}
			env.Logf("  saved target %q (client %s)", t.ID, t.ClientName)
			return nil
		},
	})
	return p, nil
}

// workerTarget is the config record of a worker add.
func workerTarget(req WorkerRequest, f *hostprobe.Facts) config.Target {
	return config.Target{ID: req.Name, Name: req.Name, SSHHost: req.Host, Hub: req.Hub.ID, WorkerName: req.Name, OS: f.OS, Arch: f.Arch, Install: "push", Service: req.Service}
}
