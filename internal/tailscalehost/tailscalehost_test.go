package tailscalehost

import (
	"errors"
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

func TestCheckStatus(t *testing.T) {
	const u = "https://hub-1.tail1234.ts.net/"
	running := func(suffix, self string) Status {
		var s Status
		s.BackendState, s.MagicDNSSuffix, s.Self.DNSName = "Running", suffix, self
		return s
	}
	cases := []struct {
		name string
		st   Status
		err  error
		ok   bool
		why  string
	}{
		{"same tailnet", running("tail1234.ts.net", "mac.tail1234.ts.net."), nil, true, ""},
		{"older client, suffix from own name", running("", "mac.tail1234.ts.net."), nil, true, ""},
		{"other tailnet", running("other-99.ts.net", "mac.other-99.ts.net."), nil, false, "tailnet other-99.ts.net"},
		{"stopped", Status{BackendState: "Stopped"}, nil, false, "is Stopped"},
		{"needs login", Status{BackendState: "NeedsLogin"}, nil, false, "is NeedsLogin"},
		{"no cli", Status{}, ErrNoCLI, false, "not installed"},
		{"daemon down", Status{}, errors.New("tailscale status: failed to connect to local Tailscale service"), false, "did not answer"},
	}
	for _, tc := range cases {
		ok, why := CheckStatus(tc.st, tc.err, u)
		if ok != tc.ok || !strings.Contains(why, tc.why) {
			t.Errorf("%s: %v %q", tc.name, ok, why)
		}
	}
}
