package tailscalehost

import (
	"strings"
	"testing"
)

func TestParseStatus(t *testing.T) {
	s, err := ParseStatus([]byte(`{"BackendState":"Running","Self":{"DNSName":"hub.tail1234.ts.net.","TailscaleIPs":["fd7a:115c:a1e0::1","100.64.0.1"]},"CertDomains":["hub.tail1234.ts.net"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Running() || s.DNSName() != "hub.tail1234.ts.net" || s.IPv4() != "100.64.0.1" || !s.HTTPSEnabled() || s.URL() != "https://hub.tail1234.ts.net/" {
		t.Fatalf("%+v", s)
	}
	off, _ := ParseStatus([]byte(`{"BackendState":"NeedsLogin","Self":{}}`))
	if off.Running() || off.HTTPSEnabled() || off.URL() != "" {
		t.Fatalf("%+v", off)
	}
}

func TestServes(t *testing.T) {
	on := []byte(`{"TCP":{"443":{"HTTPS":true}},"Web":{"hub.tail1234.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:7480"}}}}}`)
	if !Serves(on, "http://127.0.0.1:7480") {
		t.Fatal("serve config not recognized")
	}
	if !ServesHTTPS(on) || ServesHTTPS([]byte(`{}`)) || ServesHTTPS([]byte(``)) {
		t.Fatal("ServesHTTPS")
	}
	if Serves(on, "http://127.0.0.1:9999") || Serves([]byte(`{}`), "http://127.0.0.1:7480") || Serves([]byte(`not json`), "x") {
		t.Fatal("false positive")
	}
}

func TestSources(t *testing.T) {
	if got := KeyURL("ubuntu", "jammy"); got != "https://pkgs.tailscale.com/stable/ubuntu/jammy.noarmor.gpg" {
		t.Fatal(got)
	}
	if !strings.Contains(SourcesContent("debian", "bookworm"), "deb [signed-by="+Keyring+"] https://pkgs.tailscale.com/stable/debian bookworm main") {
		t.Fatal(SourcesContent("debian", "bookworm"))
	}
}
