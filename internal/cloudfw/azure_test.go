package cloudfw

import (
	"context"
	"strings"
	"testing"
)

// An NSG shaped like a typical small VM: a few allows and a source-specific deny.
var sampleRules = []Rule{
	{Name: "SSH", Priority: 300, Direction: "Inbound", Access: "Allow", Protocol: "TCP", DestinationPortRange: "22", SourceAddressPrefix: "*"},
	{Name: "HTTPS", Priority: 310, Direction: "Inbound", Access: "Allow", Protocol: "*", DestinationPortRanges: []string{"80", "443"}, SourceAddressPrefix: "*"},
	{Name: "App", Priority: 320, Direction: "Inbound", Access: "Allow", Protocol: "*", DestinationPortRanges: []string{"8080", "8443"}, SourceAddressPrefix: "*"},
	{Name: "Tunnel", Priority: 350, Direction: "Inbound", Access: "Allow", Protocol: "*", DestinationPortRanges: []string{"9000", "9001"}, SourceAddressPrefix: "*"},
	{Name: "block-scanners", Priority: 150, Direction: "Inbound", Access: "Deny", Protocol: "*", DestinationPortRange: "*", SourceAddressPrefixes: []string{"203.0.113.20", "198.51.100.11"}},
}

func TestPlanRulesPicksFreePrioritiesAfterExistingAllows(t *testing.T) {
	got, err := PlanRules(sampleRules, HubRules(7443, 7444))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Priority != 360 || got[1].Priority != 370 {
		t.Fatalf("got %+v", got)
	}
	if got[1].Ports != "7443-7444" || got[1].Protocol != "Udp" {
		t.Fatalf("udp rule %+v", got[1])
	}
}

func TestPlanRulesSkipsExistingAndRespectsCatchAllDeny(t *testing.T) {
	rules := append([]Rule{}, sampleRules...)
	rules = append(rules, Rule{Name: RulePrefix + "hub-tcp", Priority: 360, Direction: "Inbound", Access: "Allow"})
	got, err := PlanRules(rules, HubRules(7443, 7444))
	if err != nil || len(got) != 1 || got[0].Name != RulePrefix+"hub-udp" {
		t.Fatalf("got %+v err %v", got, err)
	}
	deny := []Rule{{Name: "deny-all", Priority: 400, Direction: "Inbound", Access: "Deny", SourceAddressPrefix: "*"},
		{Name: "a", Priority: 390, Direction: "Inbound", Access: "Allow"}}
	if _, err := PlanRules(deny, HubRules(7443, 7444)); err == nil {
		t.Fatal("expected no room below the catch-all deny")
	}
}

func TestCreateArgsAndParseNSG(t *testing.T) {
	ref, err := ParseNSG("/subscriptions/S1/resourceGroups/RG/providers/Microsoft.Network/networkSecurityGroups/N1")
	if err != nil || ref.Subscription != "S1" || ref.ResourceGroup != "RG" || ref.Name != "N1" {
		t.Fatalf("%+v %v", ref, err)
	}
	cmd := DisplayCommand(CreateArgs(ref, Planned{Want: HubRules(7443, 7444)[0], Priority: 360}))
	for _, want := range []string{"--subscription S1", "--nsg-name N1", "--priority 360", "--destination-port-ranges 7443", "--protocol Tcp", "--source-address-prefixes '*'"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("missing %q in %s", want, cmd)
		}
	}
	if err := Delete(context.TODO(), ref, "SSH"); err == nil {
		t.Fatal("deleting a foreign rule must be refused")
	}
}

func TestPanelRules(t *testing.T) {
	got := Names(PanelRules(true))
	if len(got) != 2 || got[0] != "lazylongarm-panel-https" || got[1] != "lazylongarm-panel-http" {
		t.Fatalf("%v", got)
	}
	if only := PanelRules(false); len(only) != 1 || only[0].Ports != "443" || only[0].Protocol != "Tcp" {
		t.Fatalf("%+v", only)
	}
}
