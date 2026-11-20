package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestPeerAddDryRunLocal(t *testing.T) {
	// config_dir points at an empty directory, so longarm (if installed) is
	// signed out and nothing reaches a real hub.
	cfg := fmt.Sprintf(`default_hub = "hub-1"

[[hubs]]
id = "hub-1"
mode = "public"
address = "hub.example.com:7443"
ca_fingerprint = "sha256:aa"
config_dir = %q
version = "v0.1.42"
`, t.TempDir())
	code, out, errOut := runWith(t, cfg, fakeTools(""), "peer", "add", "--local", "--name", "box-1", "--mint", "admin", "--service", "user", "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var p struct {
		Kind  string `json:"kind"`
		Steps []struct {
			ID string `json:"id"`
		} `json:"steps"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	var ids []string
	for _, s := range p.Steps {
		ids = append(ids, s.ID)
	}
	if p.Kind != "peer-add" || !strings.Contains(strings.Join(ids, ","), "enroll-client,try-peer,save-target") {
		t.Fatalf("plan %s %v", p.Kind, ids)
	}
	if !strings.Contains(strings.Join(p.Warnings, "\n"), "longarm admin revoke box-1-client") {
		t.Fatalf("warnings %v", p.Warnings)
	}
}

func TestPeerAddNeedsAdmin(t *testing.T) {
	cfg := fmt.Sprintf(`default_hub = "hub-1"

[[hubs]]
id = "hub-1"
mode = "public"
address = "hub.example.com:7443"
ca_fingerprint = "sha256:aa"
config_dir = %q
version = "v0.1.42"
`, t.TempDir())
	code, _, errOut := runWith(t, cfg, fakeTools(""), "peer", "add", "--local", "--name", "box-1", "--service", "user", "--dry-run")
	if code != 2 || !strings.Contains(errOut, "admin certificate") {
		t.Fatalf("non-admin peer add: %d %s", code, errOut)
	}
}
