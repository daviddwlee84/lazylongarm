package hubhost

import (
	"strings"
	"testing"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

func TestRenderIsValidTOMLWithManagedKeys(t *testing.T) {
	content := Render(Settings{PublicAddr: "hub.example.com:7443", Hosts: []string{"hub.example.com"}})
	var parsed map[string]any
	if err := toml.Unmarshal([]byte(content), &parsed); err != nil {
		t.Fatalf("invalid TOML: %v\n%s", err, content)
	}
	want := map[string]any{"listen": ":7443", "relay_udp": ":7443", "relay_stun2": ":7444", "http": "127.0.0.1:7480", "public_addr": "hub.example.com:7443", "data_dir": DataDir, "auto_approve": false}
	for k, v := range want {
		if parsed[k] != v {
			t.Errorf("%s = %v, want %v", k, parsed[k], v)
		}
	}
	if tn := parsed["tailnet"].(map[string]any); tn["provider"] != "none" {
		t.Errorf("tailnet provider must be explicit none, got %v", tn["provider"])
	}
	for _, k := range []string{"public_url", "https", "https_redirect", "acme_email"} {
		if _, ok := parsed[k]; ok {
			t.Errorf("%s must stay unset by default (panel is loopback-only)", k)
		}
	}
}

func TestRenderPublicHTTPSPanel(t *testing.T) {
	content := Render(Settings{PublicAddr: "hub.example.com:7443", Hosts: []string{"hub.example.com"},
		PublicURL: "https://hub.example.com", HTTPS: ":443", HTTPSRedirect: ":80", ACMEEmail: "ops@example.com"})
	var parsed map[string]any
	if err := toml.Unmarshal([]byte(content), &parsed); err != nil {
		t.Fatalf("invalid TOML: %v\n%s", err, content)
	}
	want := map[string]any{"http": "127.0.0.1:7480", "public_url": "https://hub.example.com", "https": ":443", "https_redirect": ":80", "acme_email": "ops@example.com"}
	for k, v := range want {
		if parsed[k] != v {
			t.Errorf("%s = %v, want %v", k, parsed[k], v)
		}
	}
	noRedirect := Render(Settings{PublicAddr: "h:7443", PublicURL: "https://h", HTTPS: ":443"})
	if strings.Contains(noRedirect, "https_redirect") || strings.Contains(noRedirect, "acme_email") {
		t.Errorf("optional keys rendered:\n%s", noRedirect)
	}
}

func TestParseBootstrap(t *testing.T) {
	exp := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
	st := ParseBootstrap("# longarm-hub bootstrap\nboot_id=abc\nca_fingerprint=sha256:ff\nhub_address=h:7443\nadmin_joined=false\nadmin_join_code=CODE-1\nadmin_join_code_expires=" + exp + "\n")
	if st.Fingerprint != "sha256:ff" || st.AdminCode != "CODE-1" || !st.CodeUsable(time.Now()) {
		t.Fatalf("%+v", st)
	}
	st.AdminJoined = true
	if st.CodeUsable(time.Now()) {
		t.Fatal("code after admin joined must not be used")
	}
}

func TestParseFingerprintAndHeredoc(t *testing.T) {
	if fp := ParseFingerprint("subject     hub CA\nnot after   2036\nfingerprint sha256:abcd\n"); fp != "sha256:abcd" {
		t.Fatalf("fp %q", fp)
	}
	body, delim := Heredoc("contains LAZYLONGARM_EOF inside")
	if strings.Contains(body, delim+"\n") || delim == "LAZYLONGARM_EOF" {
		t.Fatalf("delimiter %q collides", delim)
	}
}

func TestCodeUsableNeedsTheCode(t *testing.T) {
	exp := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
	st := ParseBootstrap("admin_joined=false\nadmin_join_code=C\nadmin_join_code_expires=" + exp + "\n")
	if !st.CodeUsable(time.Now()) {
		t.Fatal("fresh code unusable")
	}
}

func TestParseSettingsRoundTrip(t *testing.T) {
	in := Settings{Port: 8443, Stun2: 8444, PublicAddr: "hub.example.com:8443", Hosts: []string{"hub.example.com"}, AutoApprove: true, RelayRate: "8M"}
	content := Render(in)
	s, edited, err := ParseSettings(content)
	if err != nil {
		t.Fatal(err)
	}
	if edited || s.Port != 8443 || s.Stun2 != 8444 || !s.AutoApprove || s.RelayRate != "8M" || s.Panel != "127.0.0.1:7480" {
		t.Fatalf("edited=%v %+v", edited, s)
	}
	pub := s
	pub.PublicURL, pub.HTTPS, pub.HTTPSRedirect = "https://hub.example.com", ":443", ":80"
	back, edited, err := ParseSettings(Render(pub))
	if err != nil || edited || back.HTTPS != ":443" || back.PublicURL != "https://hub.example.com" || back.HTTPSRedirect != ":80" {
		t.Fatalf("%v %v %+v", err, edited, back)
	}
	if _, edited, _ := ParseSettings(content + "\n[scheduler]\ngate_free_gb = 2.0\n"); !edited {
		t.Fatal("hand edit not detected")
	}
	if _, _, err := ParseSettings("name = \"hub\"\n"); err == nil {
		t.Fatal("missing public_addr accepted")
	}
}
