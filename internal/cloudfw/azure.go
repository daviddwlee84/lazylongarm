// Package cloudfw opens the hub's ports in a cloud firewall through the
// provider's own CLI (Azure: `az`). It only creates rules it names with a
// lazylongarm prefix and records them, so status and uninstall touch exactly
// those rules.
package cloudfw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

var commandContext = exec.CommandContext

// RulePrefix marks rules lazylongarm owns.
const RulePrefix = "lazylongarm-"

// AzureVM is a VM found by FQDN or public IP, with the NSGs that filter its
// traffic (NIC-level and subnet-level; both must allow a flow).
type AzureVM struct {
	Subscription  string   `json:"subscription"`
	ResourceGroup string   `json:"resource_group"`
	Name          string   `json:"name"`
	FQDNs         string   `json:"fqdns"`
	PublicIPs     string   `json:"public_ips"`
	NIC           string   `json:"nic"`
	NSGs          []string `json:"nsgs"` // full resource IDs
}

// Rule is an NSG security rule (subset).
type Rule struct {
	Name                  string   `json:"name"`
	Priority              int      `json:"priority"`
	Direction             string   `json:"direction"`
	Access                string   `json:"access"`
	Protocol              string   `json:"protocol"`
	DestinationPortRange  string   `json:"destinationPortRange"`
	DestinationPortRanges []string `json:"destinationPortRanges"`
	SourceAddressPrefix   string   `json:"sourceAddressPrefix"`
	SourceAddressPrefixes []string `json:"sourceAddressPrefixes"`
	Description           string   `json:"description"`
}

// Want is a rule lazylongarm needs.
type Want struct {
	Name     string
	Protocol string // Tcp | Udp
	Ports    string // "7443" or "7443-7444"
	Purpose  string
}

// HubRules are the inbound rules a public longarm-hub needs.
func HubRules(port, stun2 int) []Want {
	udp := strconv.Itoa(port)
	if stun2 == port+1 {
		udp = fmt.Sprintf("%d-%d", port, stun2)
	} else if stun2 > 0 {
		udp = fmt.Sprintf("%d,%d", port, stun2)
	}
	return []Want{
		{Name: RulePrefix + "hub-tcp", Protocol: "Tcp", Ports: strconv.Itoa(port), Purpose: "longarm-hub gRPC (mTLS) + relay TLS fallback"},
		{Name: RulePrefix + "hub-udp", Protocol: "Udp", Ports: udp, Purpose: "longarm-hub relay QUIC + STUN"},
	}
}

// PanelRules are the inbound rules the built-in HTTPS panel needs: 443/tcp
// (TLS-ALPN-01 certificate checks + the panel) and, with redirect, 80/tcp.
func PanelRules(redirect bool) []Want {
	out := []Want{{Name: RulePrefix + "panel-https", Protocol: "Tcp", Ports: "443", Purpose: "longarm-hub web panel over HTTPS (Let's Encrypt TLS-ALPN-01)"}}
	if redirect {
		out = append(out, Want{Name: RulePrefix + "panel-http", Protocol: "Tcp", Ports: "80", Purpose: "longarm-hub web panel: http -> https redirect"})
	}
	return out
}

// Names lists the rule names of wants.
func Names(wants []Want) []string {
	out := make([]string, 0, len(wants))
	for _, w := range wants {
		out = append(out, w.Name)
	}
	return out
}

func az(ctx context.Context, v any, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := commandContext(ctx, "az", append(args, "--only-show-errors")...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.Is(err, exec.ErrNotFound) {
			return errors.New("the Azure CLI (az) is not installed")
		}
		msg := strings.TrimSpace(errb.String())
		if strings.Contains(msg, "az login") {
			return fmt.Errorf("Azure CLI is not logged in: run `az login` (%s)", firstLine(msg))
		}
		if errors.As(err, &exit) {
			return fmt.Errorf("az %s: %s", args[0]+" "+args[1], firstLine(msg))
		}
		return err
	}
	if v == nil {
		return nil
	}
	if err := json.Unmarshal(out.Bytes(), v); err != nil {
		return fmt.Errorf("decode az output: %w", err)
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// FindAzureVM searches every enabled subscription for a VM whose FQDN or
// public IP equals host.
func FindAzureVM(ctx context.Context, host string) (*AzureVM, error) {
	var subs []struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := az(ctx, &subs, "account", "list", "-o", "json"); err != nil {
		return nil, err
	}
	isIP := net.ParseIP(host) != nil
	for _, s := range subs {
		if s.State != "Enabled" {
			continue
		}
		var vms []struct {
			Name string `json:"name"`
			RG   string `json:"rg"`
			FQDN string `json:"fqdns"`
			IPs  string `json:"ips"`
			NIC  string `json:"nic"`
		}
		err := az(ctx, &vms, "vm", "list", "-d", "--subscription", s.ID, "-o", "json",
			"--query", "[].{name:name, rg:resourceGroup, fqdns:fqdns, ips:publicIps, nic:networkProfile.networkInterfaces[0].id}")
		if err != nil {
			continue
		}
		for _, vm := range vms {
			match := false
			for _, f := range strings.Split(vm.FQDN, ",") {
				if strings.EqualFold(strings.TrimSpace(f), host) {
					match = true
				}
			}
			if isIP {
				for _, ip := range strings.Split(vm.IPs, ",") {
					if strings.TrimSpace(ip) == host {
						match = true
					}
				}
			}
			if !match {
				continue
			}
			out := &AzureVM{Subscription: s.ID, ResourceGroup: vm.RG, Name: vm.Name, FQDNs: vm.FQDN, PublicIPs: vm.IPs, NIC: vm.NIC}
			var nic struct {
				NSG    string `json:"nsg"`
				Subnet string `json:"subnet"`
			}
			if err := az(ctx, &nic, "network", "nic", "show", "--ids", vm.NIC, "-o", "json",
				"--query", "{nsg:networkSecurityGroup.id, subnet:ipConfigurations[0].subnet.id}"); err != nil {
				return nil, err
			}
			if nic.NSG != "" {
				out.NSGs = append(out.NSGs, nic.NSG)
			}
			if nic.Subnet != "" {
				var subnetNSG string
				if err := az(ctx, &subnetNSG, "network", "vnet", "subnet", "show", "--ids", nic.Subnet, "-o", "json", "--query", "networkSecurityGroup.id"); err == nil && subnetNSG != "" {
					out.NSGs = append(out.NSGs, subnetNSG)
				}
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("no Azure VM with FQDN or public IP %s in any enabled subscription", host)
}

// ListRules returns the NSG's custom rules.
func ListRules(ctx context.Context, nsgID string) ([]Rule, error) {
	var rules []Rule
	err := az(ctx, &rules, "network", "nsg", "show", "--ids", nsgID, "-o", "json", "--query", "securityRules")
	return rules, err
}

// NSGRef splits an NSG resource ID.
type NSGRef struct{ Subscription, ResourceGroup, Name string }

// ParseNSG parses /subscriptions/S/resourceGroups/RG/providers/Microsoft.Network/networkSecurityGroups/NAME.
func ParseNSG(id string) (NSGRef, error) {
	parts := strings.Split(strings.Trim(id, "/"), "/")
	var r NSGRef
	for i := 0; i+1 < len(parts); i++ {
		switch strings.ToLower(parts[i]) {
		case "subscriptions":
			r.Subscription = parts[i+1]
		case "resourcegroups":
			r.ResourceGroup = parts[i+1]
		case "networksecuritygroups":
			r.Name = parts[i+1]
		}
	}
	if r.Subscription == "" || r.ResourceGroup == "" || r.Name == "" {
		return r, fmt.Errorf("not an NSG resource id: %s", id)
	}
	return r, nil
}

// Planned is a rule to create.
type Planned struct {
	Want
	Priority int
}

// PlanRules returns the wanted rules missing from existing, each with a free
// priority that sorts before any catch-all Deny covering inbound traffic.
func PlanRules(existing []Rule, wants []Want) ([]Planned, error) {
	used := map[int]bool{}
	have := map[string]bool{}
	ceiling := 4096
	maxAllow := 299
	for _, r := range existing {
		if !strings.EqualFold(r.Direction, "Inbound") {
			continue
		}
		used[r.Priority] = true
		have[r.Name] = true
		if strings.EqualFold(r.Access, "Deny") && catchAllSource(r) && r.Priority < ceiling {
			ceiling = r.Priority
		}
		if strings.EqualFold(r.Access, "Allow") && r.Priority > maxAllow && r.Priority < ceiling {
			maxAllow = r.Priority
		}
	}
	var out []Planned
	next := (maxAllow/10 + 1) * 10
	for _, w := range wants {
		if have[w.Name] {
			continue
		}
		for used[next] && next < ceiling {
			next += 10
		}
		if next >= ceiling {
			return nil, fmt.Errorf("no free NSG priority below the catch-all Deny rule at %d", ceiling)
		}
		used[next] = true
		out = append(out, Planned{Want: w, Priority: next})
		next += 10
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out, nil
}

func catchAllSource(r Rule) bool {
	srcs := append([]string{r.SourceAddressPrefix}, r.SourceAddressPrefixes...)
	for _, s := range srcs {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "*", "internet", "any", "0.0.0.0/0":
			return true
		}
	}
	return false
}

// CreateArgs is the exact `az` command for a planned rule.
func CreateArgs(ref NSGRef, p Planned) []string {
	args := []string{"network", "nsg", "rule", "create",
		"--subscription", ref.Subscription, "--resource-group", ref.ResourceGroup, "--nsg-name", ref.Name,
		"--name", p.Name, "--priority", strconv.Itoa(p.Priority), "--direction", "Inbound", "--access", "Allow",
		"--protocol", p.Protocol, "--source-address-prefixes", "*", "--source-port-ranges", "*",
		"--destination-address-prefixes", "*", "--destination-port-ranges"}
	args = append(args, strings.Split(p.Ports, ",")...)
	return append(args, "--description", "lazylongarm: "+p.Purpose)
}

// Create runs the `az` command for p.
func Create(ctx context.Context, ref NSGRef, p Planned) error {
	return az(ctx, nil, append(CreateArgs(ref, p), "-o", "none")...)
}

// Delete removes a lazylongarm-owned rule.
func Delete(ctx context.Context, ref NSGRef, name string) error {
	if !strings.HasPrefix(name, RulePrefix) {
		return fmt.Errorf("refusing to delete %s: not a lazylongarm rule", name)
	}
	return az(ctx, nil, "network", "nsg", "rule", "delete", "--subscription", ref.Subscription, "--resource-group", ref.ResourceGroup, "--nsg-name", ref.Name, "--name", name)
}

// DisplayCommand renders args as a copyable shell command.
func DisplayCommand(args []string) string {
	var b strings.Builder
	b.WriteString("az")
	for _, a := range args {
		b.WriteByte(' ')
		if a == "" || strings.ContainsAny(a, " *'\"$`\\;&|<>()") {
			b.WriteString("'" + strings.ReplaceAll(a, "'", `'\''`) + "'")
		} else {
			b.WriteString(a)
		}
	}
	return b.String()
}
