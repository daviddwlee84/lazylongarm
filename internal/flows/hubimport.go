package flows

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/hubshare"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
)

// ImportRequest describes `hub import`: register a hub in this machine's
// config, from a `hub share` payload or from this machine's longarm.
type ImportRequest struct {
	Payload   hubshare.Payload
	Source    string // "stdin" (hub share) or "longarm" (this machine's client)
	Config    *config.Config
	VerifySSH bool // check `ssh <hub ssh host> true` afterwards
}

// BuildHubImport returns the plan; the merge itself runs under the config
// lock in Apply, against the file as it is then.
func BuildHubImport(req ImportRequest) (*plan.Plan, error) {
	h := req.Payload.Hub
	if h.ID == "" || h.Address == "" {
		return nil, fmt.Errorf("no hub id or address to import")
	}
	p := &plan.Plan{Kind: "hub-import", Key: h.ID, Title: fmt.Sprintf("Register hub %s in this machine's lazylongarm config", h.ID)}
	p.Summary = append(p.Summary,
		[2]string{"hub", fmt.Sprintf("%s at %s (%s)", h.ID, h.Address, h.Mode)},
		[2]string{"CA", shortFP(h.CAFingerprint)},
		[2]string{"ssh host", dashIf(h.SSHHost)},
		[2]string{"source", req.Source},
	)
	if req.Payload.From != "" {
		p.Summary = append(p.Summary, [2]string{"shared by", req.Payload.From})
	}
	if len(req.Payload.Targets) > 0 {
		p.Summary = append(p.Summary, [2]string{"targets", fmt.Sprintf("%d", len(req.Payload.Targets))})
	}
	if h.SSHHost == "" {
		p.Warnings = append(p.Warnings, "no ssh host: hub-host actions (status of the service, panel, panel-user, backup) stay off until you import again with --ssh-host")
	}
	if req.Config != nil {
		if mine, err := hubshare.LocalMembership(h.ConfigDir); err == nil && mine.Joined() {
			if err := mine.Matches(h); err != nil {
				p.Warnings = append(p.Warnings, "this machine's longarm is joined elsewhere: "+err.Error())
			}
		} else {
			p.Warnings = append(p.Warnings, "this machine's longarm is not joined to this hub: Fleet and Jobs stay empty until it is (`lazylongarm peer add <this host>` from a machine that operates the hub)")
		}
	}

	p.Steps = append(p.Steps, plan.Step{
		ID: "register", Title: "Add or update the hub entry", Idempotent: true,
		Actions: []plan.Action{{Where: "local", Display: "merge into lazylongarm's config (same address + CA: update; keeps this machine's config_dir)"}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			if req.Config == nil {
				return fmt.Errorf("no config")
			}
			var res hubshare.MergeResult
			err := req.Config.Update(func(c *config.Config) error {
				var err error
				res, err = hubshare.Merge(c, req.Payload)
				return err
			})
			if err != nil {
				return err
			}
			b, _ := json.Marshal(res)
			env.Set("merge", string(b))
			env.Logf("  hub %s %s (default: %v); targets +%d ~%d", res.HubID, res.Hub, res.Default, len(res.TargetsAdded), len(res.TargetsUpdated))
			return nil
		},
	})
	if req.VerifySSH && h.SSHHost != "" {
		p.Steps = append(p.Steps, plan.Step{
			ID: "ssh-hub", Title: "Reach the hub host over ssh", Idempotent: true, ReadOnly: true,
			Actions: []plan.Action{{Where: h.SSHHost, Display: "true"}},
			Apply: func(ctx context.Context, env *plan.Env) error {
				_, err := env.RunQuiet(ctx, connection.Request{Host: h.SSHHost, Script: "true", Timeout: 20 * time.Second})
				return err
			},
		})
	}
	return p, nil
}
