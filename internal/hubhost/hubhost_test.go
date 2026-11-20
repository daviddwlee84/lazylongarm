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
	if _, ok := parsed["public_url"]; ok {
		t.Error("public_url must stay unset (panel is loopback-only)")
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
