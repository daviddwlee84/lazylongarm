// Package fleet merges what the hub reports with what lazylongarm manages,
// for both `worker ls`/`hub status` and the dashboard.
package fleet

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/cloudfw"
	"github.com/daviddwlee84/lazylongarm/internal/cloudvm"
	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/flows"
	"github.com/daviddwlee84/lazylongarm/internal/hubhost"
	"github.com/daviddwlee84/lazylongarm/internal/longarm"
	"github.com/daviddwlee84/lazylongarm/internal/tailscalehost"
)

// Row merges a configured target with the hub's view.
type Row struct {
	Name     string  `json:"name"`
	SSHHost  string  `json:"ssh_host,omitempty"`
	Managed  bool    `json:"managed"` // lazylongarm installed it
	InHub    bool    `json:"in_hub"`
	Online   bool    `json:"online"`
	State    string  `json:"state"`
	Platform string  `json:"platform,omitempty"`
	Version  string  `json:"version,omitempty"`
	Path     string  `json:"path,omitempty"`
	CPU      float64 `json:"cpu_percent"`
	Mem      float64 `json:"mem_percent"`
	Jobs     int     `json:"jobs_active"`
	Owner    string  `json:"owner,omitempty"`
	LastSeen string  `json:"last_seen,omitempty"`
	// Labels from the hub registry (worker add --label, plus os/arch).
	Labels map[string]string `json:"labels,omitempty"`
}

// LabelText is the labels as sorted k=v pairs, without os/arch (shown as the
// platform already).
func (r Row) LabelText() string {
	keys := make([]string, 0, len(r.Labels))
	for k := range r.Labels {
		if k != "os" && k != "arch" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + r.Labels[k]
	}
	return strings.Join(parts, " ")
}

// Merge combines the hub registry with lazylongarm-managed targets.
func Merge(cfg *config.Config, nodes []longarm.Node) []Row {
	byName := map[string]*Row{}
	var order []string
	for _, n := range nodes {
		if !n.IsWorker() {
			continue
		}
		state := n.State
		if state == "approved" {
			state = map[bool]string{true: "online", false: "offline"}[n.Online]
		}
		r := &Row{Name: n.Name, InHub: true, Online: n.Online, State: state, Platform: n.OS + "/" + n.Arch, Version: n.Version, Path: n.Path(), Jobs: n.JobsActive, Owner: n.Owner, LastSeen: n.LastSeen, Labels: n.Labels}
		if n.Metrics != nil {
			r.CPU, r.Mem = n.Metrics.CPUPercent, n.MemPercent()
		}
		// A re-enrolled machine is listed twice: its revoked old node and
		// the new one. Show one row per name, the live node when there is one.
		if old, ok := byName[n.Name]; ok {
			if n.State == "revoked" || old.State != "revoked" {
				continue
			}
			byName[n.Name] = r
			continue
		}
		byName[n.Name] = r
		order = append(order, n.Name)
	}
	if cfg != nil {
		for _, t := range cfg.Targets {
			name := t.WorkerName
			if name == "" {
				name = t.ID
			}
			if r, ok := byName[name]; ok {
				r.Managed, r.SSHHost = true, t.SSHHost
				continue
			}
			byName[name] = &Row{Name: name, SSHHost: t.SSHHost, Managed: true, State: "not in hub", Platform: t.OS + "/" + t.Arch}
			order = append(order, name)
		}
	}
	out := make([]Row, 0, len(order))
	for _, n := range order {
		out = append(out, *byName[n])
	}
	return out
}

// HubCheck is one line of `hub status`.
type HubCheck struct {
	ID     string `json:"id"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// HubStatus checks the hub service (over SSH), reachability, the cloud
// firewall rules lazylongarm created and this machine's enrollment.
func HubStatus(ctx context.Context, h *config.Hub) []HubCheck {
	var checks []HubCheck
	add := func(id, status, detail, fix string) { checks = append(checks, HubCheck{id, status, detail, fix}) }
	if h.SSHHost != "" {
		res, err := connection.Run(ctx, connection.Request{Host: h.SSHHost, Script: `systemctl is-active longarm-hub; dpkg-query -W -f='${Version}\n' longarm-hub 2>/dev/null; curl -fsS --max-time 3 "http://$1/healthz" 2>/dev/null || echo unhealthy`, Args: []string{orDefault(h.Panel, "127.0.0.1:7480")}, Timeout: 30 * time.Second})
		lines := strings.Split(strings.TrimSpace(string(res.Stdout)), "\n")
		switch {
		case err != nil && len(lines) < 1:
			add("service", "fail", err.Error(), "ssh "+h.SSHHost)
		default:
			state := lines[0]
			ver := ""
			if len(lines) > 1 {
				ver = lines[1]
			}
			if state == "active" {
				add("service", "ok", "longarm-hub.service active, package "+dash(ver), "")
			} else {
				add("service", "fail", "longarm-hub.service "+state, "ssh "+h.SSHHost+" sudo journalctl -u longarm-hub -n 50")
			}
			if len(lines) > 2 {
				health := strings.Join(lines[2:], " ")
				if hubhost.HealthOK(health) {
					add("healthz", "ok", hubhost.HealthSummary(health), "")
				} else {
					add("healthz", "fail", hubhost.HealthSummary(health), "ssh "+h.SSHHost+" sudo journalctl -u longarm-hub -n 50")
				}
			}
		}
	}
	if h.SSHHost != "" {
		if users, err := PanelUsers(ctx, h.SSHHost); err != nil {
			add("panel", "warn", "cannot list panel logins: "+firstLine(err.Error()), "")
		} else if len(users) == 0 {
			add("panel", "warn", "no web panel login yet", "lazylongarm hub panel-user (U in the dashboard)")
		} else {
			add("panel", "ok", fmt.Sprintf("%d login(s): %s", len(users), strings.Join(users, ", ")), "")
		}
	}
	if c, ok := panelAccessCheck(ctx, h); ok {
		checks = append(checks, c)
	}
	if err := flows.DialTCP(ctx, h.Address); err != nil {
		add("reachable", "fail", "TCP "+h.Address+": "+err.Error(), "check the firewall / NSG and proxy TUN rules")
	} else {
		add("reachable", "ok", "TCP "+h.Address+" accepts connections from this machine", "")
	}
	if h.Cloud != nil && h.Cloud.Provider == "azure" && h.Cloud.VM != "" {
		vm, err := cloudvm.Show(ctx, cloudvm.Ref{Subscription: h.Cloud.Subscription, ResourceGroup: h.Cloud.ResourceGroup, Name: h.Cloud.VM})
		switch {
		case err != nil:
			add("vm", "warn", "cannot read the VM: "+firstLine(err.Error()), "az login")
		case vm.Running():
			add("vm", "ok", fmt.Sprintf("%s · %s · %s · %s", vm.Power, vm.Size, vm.Location, vm.PublicIPs), "")
		default:
			add("vm", "fail", fmt.Sprintf("%s · %s · %s", vm.Power, vm.Size, vm.Location), "lazylongarm hub vm start")
		}
	}
	if h.Cloud != nil && h.Cloud.Provider == "azure" && h.Cloud.NSG != "" {
		rules, err := cloudfw.ListRules(ctx, h.Cloud.NSG)
		if err != nil {
			add("firewall", "warn", "cannot list NSG rules: "+err.Error(), "az login")
		} else {
			have := map[string]bool{}
			for _, r := range rules {
				have[r.Name] = true
			}
			var missing []string
			for _, name := range h.Cloud.Rules {
				if !have[name] {
					missing = append(missing, name)
				}
			}
			if len(missing) == 0 {
				add("firewall", "ok", "NSG rules present: "+strings.Join(h.Cloud.Rules, ", "), "")
			} else {
				add("firewall", "fail", "NSG rules missing: "+strings.Join(missing, ", "), "lazylongarm hub setup (reruns the firewall step)")
			}
		}
	}
	client := &longarm.Client{ConfigDir: h.ConfigDir}
	if w, err := client.WhoAmI(ctx); err != nil {
		add("client", "warn", "this machine is not enrolled: "+firstLine(err.Error()), "lazylongarm hub setup (enroll step)")
	} else {
		add("client", "ok", fmt.Sprintf("enrolled as %s (%s) on %q, certificate until %s", w.Name, w.Role, w.HubName, w.CertNotAfter), "")
	}
	return checks
}

// panelAccessCheck verifies the tailnet/public panel URL `hub
// panel-access` set up; loopback-only hubs have no extra check.
func panelAccessCheck(ctx context.Context, h *config.Hub) (HubCheck, bool) {
	c := HubCheck{ID: "panel-access"}
	switch h.PanelMode() {
	case config.PanelPublic:
		url := h.PanelURL()
		if err := flows.HTTPSGet(ctx, url+"healthz"); err != nil {
			c.Status, c.Detail, c.Fix = "fail", "public "+url+": "+firstLine(err.Error()), "lazylongarm hub panel-access public (reruns the checks)"
			return c, true
		}
		c.Status, c.Detail = "ok", "public "+url+" (HTTPS)"
		if left, err := certDaysLeft(ctx, h.PanelAccess.Domain); err == nil {
			c.Detail += fmt.Sprintf(", certificate valid %d more days", left)
			if left < 14 {
				c.Status, c.Fix = "warn", "longarm-hub renews it itself; check: ssh "+h.SSHHost+" sudo journalctl -u longarm-hub | grep -i acme"
			}
		}
	case config.PanelTailnet:
		if h.SSHHost == "" {
			return c, false
		}
		res, err := connection.Run(ctx, connection.Request{Host: h.SSHHost, Sudo: true, Timeout: 30 * time.Second, Script: `tailscale serve status --json 2>/dev/null || true`})
		switch {
		case err != nil:
			c.Status, c.Detail = "warn", "cannot read tailscale serve status: "+firstLine(err.Error())
		case tailscalehost.Serves(res.Stdout, "http://"+orDefault(h.Panel, "127.0.0.1:7480")):
			c.Status, c.Detail = "ok", "tailnet "+h.PanelURL()+" (tailscale serve)"
		default:
			c.Status, c.Detail, c.Fix = "fail", "tailscale serve no longer proxies the panel", "lazylongarm hub panel-access tailnet"
		}
	default:
		return c, false
	}
	return c, true
}

func certDaysLeft(ctx context.Context, domain string) (int, error) {
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: &tls.Config{ServerName: domain, MinVersion: tls.VersionTLS12}}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(domain, "443"))
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return 0, fmt.Errorf("no certificate")
	}
	return int(time.Until(certs[0].NotAfter).Hours() / 24), nil
}

// PanelUsers lists the hub's web panel logins (`longarm-hub user ls`).
func PanelUsers(ctx context.Context, sshHost string) ([]string, error) {
	res, err := connection.Run(ctx, connection.Request{Host: sshHost, Sudo: true, Timeout: 30 * time.Second,
		Script: `runuser -u longarm-hub -- /usr/bin/longarm-hub --config /etc/longarm-hub/hub.toml user ls`})
	if err != nil {
		return nil, err
	}
	var users []string
	for _, l := range strings.Split(strings.TrimSpace(string(res.Stdout)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			users = append(users, l)
		}
	}
	return users, nil
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
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
