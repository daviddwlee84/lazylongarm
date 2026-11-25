package connection

import "testing"

func TestParseSSHG(t *testing.T) {
	tg := ParseSSHG([]byte("user alice\nhostname hub.example.com\nport 22\nproxyjump none\nidentityfile ~/.ssh/id_ed25519\n"))
	if tg.Hostname != "hub.example.com" || tg.User != "alice" || !tg.Direct() || tg.UserHost() != "alice@hub.example.com" || tg.KnownHostsKey() != "hub.example.com" {
		t.Fatalf("%+v", tg)
	}
	tg = ParseSSHG([]byte("hostname 10.0.0.5\nport 2222\nproxyjump bastion\n"))
	if tg.Direct() || tg.KnownHostsKey() != "[10.0.0.5]:2222" || tg.UserHost() != "10.0.0.5" {
		t.Fatalf("%+v", tg)
	}
}
