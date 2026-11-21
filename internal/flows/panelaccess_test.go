package flows

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/hostprobe"
	"github.com/daviddwlee84/lazylongarm/internal/hubhost"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
)

const testNSG = "/subscriptions/s1/resourceGroups/rg1/providers/Microsoft.Network/networkSecurityGroups/nsg1"

func hubHost() *hostprobe.Facts {
	f, _ := hostprobe.Parse(`os=Linux
arch=x86_64
distro_id=ubuntu
distro_codename=jammy
distro_pretty=Ubuntu 22.04.2 LTS
sudo=nopasswd
has_systemctl=yes
has_apt-get=yes
has_curl=yes
has_gpg=yes
listen=tcp 0.0.0.0:7443
svc_longarm_hub=active
probe_done=yes
`)
	return f
}

func testHub(mode string, pa *config.PanelAccess) *config.Hub {
	return &config.Hub{ID: "hub-1", Name: "hub-1", Mode: mode, Address: "hub.example.com:7443", SSHHost: "my-vps", Panel: "127.0.0.1:7480",
		Cloud: &config.Cloud{Provider: "azure", NSG: testNSG, Rules: []string{"lazylongarm-hub-tcp", "lazylongarm-hub-udp"}}, PanelAccess: pa}
}

func currentHubTOML(https bool) string {
	s := hubhost.Settings{PublicAddr: "hub.example.com:7443", Hosts: []string{"hub.example.com"}, AutoApprove: true}
	if https {
		s.PublicURL, s.HTTPS, s.HTTPSRedirect = "https://hub.example.com", ":443", ":80"
	}
	return hubhost.Render(s)
}

func render(p *plan.Plan) string {
	var b strings.Builder
	p.Render(&b)
	return b.String()
}

func TestPanelAccessTailnetPlan(t *testing.T) {
	p, err := BuildPanelAccess(PanelAccessRequest{Hub: testHub(config.ModePublic, nil), Mode: config.PanelTailnet}, hubHost(), PanelAccessDeps{})
	if err != nil {
		t.Fatal(err)
	}
	want := "tailscale-repo,install-tailscale,tailscaled,tailscale-login,tailscale-serve,tailnet-url,save-panel-access"
	if got := ids(p); got != want {
		t.Fatalf("steps\n got %s\nwant %s", got, want)
	}
	out := render(p)
	for _, s := range []string{"pkgs.tailscale.com/stable/ubuntu/jammy.noarmor.gpg", "2596A99EAAB33821893C0A79458CA832957F5868", "--accept-dns=false", "tailscale serve --bg --https=443 http://127.0.0.1:7480", "never funnel"} {
		if !strings.Contains(out, s) {
			t.Errorf("review lacks %q", s)
		}
	}
	if strings.Contains(out, "funnel --") || strings.Contains(out, "tailscale funnel") {
		t.Error("tailnet mode must never use funnel")
	}
}

func TestPanelAccessTailnetFromPublicUndoesPublic(t *testing.T) {
	h := testHub(config.ModePublic, &config.PanelAccess{Mode: config.PanelPublic, Domain: "hub.example.com", URL: "https://hub.example.com/", Redirect: true})
	p, err := BuildPanelAccess(PanelAccessRequest{Hub: h, Mode: config.PanelTailnet, HubConfig: currentHubTOML(true)}, hubHost(), PanelAccessDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(p); !strings.HasPrefix(got, "hub-config,remove-bind-capability,restart-hub,panel-firewall-close,tailscale-repo") {
		t.Fatalf("public mode not undone first: %s", got)
	}
}

func TestPanelAccessPublicPlanAndValidation(t *testing.T) {
	h := testHub(config.ModePublic, nil)
	busy := hubHost()
	busy.Listening = append(busy.Listening, "tcp 0.0.0.0:443")
	req := PanelAccessRequest{Hub: h, Mode: config.PanelPublic, Redirect: true, HubConfig: currentHubTOML(false)}
	if _, err := BuildPanelAccess(req, busy, PanelAccessDeps{}); err == nil || !strings.Contains(err.Error(), "tcp/443") {
		t.Fatalf("busy 443 not detected: %v", err)
	}
	p, err := BuildPanelAccess(req, hubHost(), PanelAccessDeps{})
	if err != nil {
		t.Fatal(err)
	}
	want := "bind-capability,hub-config,restart-hub,panel-firewall,https-reachable,save-panel-access"
	if got := ids(p); got != want {
		t.Fatalf("steps\n got %s\nwant %s", got, want)
	}
	out := render(p)
	for _, s := range []string{`public_url = "https://hub.example.com"`, `https = ":443"`, `https_redirect = ":80"`, "auto_approve = true", "AmbientCapabilities=CAP_NET_BIND_SERVICE", "password + TOTP"} {
		if !strings.Contains(out, s) {
			t.Errorf("review lacks %q", s)
		}
	}
	noRedirect := req
	noRedirect.Redirect = false
	p, err = BuildPanelAccess(noRedirect, busy2(), PanelAccessDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(p); !strings.Contains(got, "panel-firewall,panel-http-close,https-reachable") || strings.Contains(render(p), "https_redirect") {
		t.Fatalf("no-redirect plan: %s", got)
	}

	for name, bad := range map[string]PanelAccessRequest{
		"private hub": {Hub: testHub(config.ModePrivate, nil), Mode: config.PanelPublic, HubConfig: currentHubTOML(false)},
		"ip domain":   {Hub: h, Mode: config.PanelPublic, Domain: "203.0.113.10", HubConfig: currentHubTOML(false)},
		"url domain":  {Hub: h, Mode: config.PanelPublic, Domain: "https://hub.example.com", HubConfig: currentHubTOML(false)},
		"no config":   {Hub: h, Mode: config.PanelPublic},
		"no ssh":      {Hub: &config.Hub{ID: "x", Mode: config.ModePublic, Address: "x:7443"}, Mode: config.PanelTailnet},
	} {
		if _, err := BuildPanelAccess(bad, hubHost(), PanelAccessDeps{}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// busy2 has port 80 taken, which only matters with the redirect.
func busy2() *hostprobe.Facts {
	f := hubHost()
	f.Listening = append(f.Listening, "tcp 0.0.0.0:80")
	return f
}

func TestPanelAccessLoopbackRevertsEverything(t *testing.T) {
	h := testHub(config.ModePublic, &config.PanelAccess{Mode: config.PanelPublic, Domain: "hub.example.com", URL: "https://hub.example.com/", Redirect: true})
	p, err := BuildPanelAccess(PanelAccessRequest{Hub: h, Mode: config.PanelLoopback, HubConfig: currentHubTOML(true)}, hubHost(), PanelAccessDeps{})
	if err != nil {
		t.Fatal(err)
	}
	want := "tailscale-serve-off,hub-config,remove-bind-capability,restart-hub,panel-firewall-close,save-panel-access"
	if got := ids(p); got != want {
		t.Fatalf("steps\n got %s\nwant %s", got, want)
	}
	out := render(p)
	if strings.Contains(out, `https = ":443"`) || !strings.Contains(out, "auto_approve = true") {
		t.Fatalf("revert must drop https and keep the other settings:\n%s", out)
	}
}

func TestTailscaleLoginHandsOverTerminal(t *testing.T) {
	p, err := BuildPanelAccess(PanelAccessRequest{Hub: testHub(config.ModePublic, nil), Mode: config.PanelTailnet}, hubHost(), PanelAccessDeps{})
	if err != nil {
		t.Fatal(err)
	}
	var login plan.Step
	for _, s := range p.Steps {
		if s.ID == "tailscale-login" {
			login = s
		}
	}
	env := plan.NewEnv()
	var fixErr *plan.FixError
	if err := login.Apply(context.Background(), env); !errors.As(err, &fixErr) || !strings.Contains(fixErr.Fix, "--auth-key-file") {
		t.Fatalf("no terminal: %v", err)
	}
	var got connection.Request
	env.Interactive = func(_ context.Context, r connection.Request) error { got = r; return nil }
	if err := login.Apply(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if got.Host != "my-vps" || !got.Sudo || !strings.Contains(got.Script, "tailscale up") || len(got.Args) != 1 || got.Args[0] != "hub-1" {
		t.Fatalf("%+v", got)
	}
}

func TestSavePanelAccessAndHubSetupKeepsIt(t *testing.T) {
	cfg, _ := config.Load(filepath.Join(t.TempDir(), "c.toml"), false)
	h := testHub(config.ModePublic, nil)
	cfg.UpsertHub(*h)
	if err := cfg.Save(); err != nil { // the save step re-reads the file
		t.Fatal(err)
	}
	p, err := BuildPanelAccess(PanelAccessRequest{Hub: h, Mode: config.PanelPublic, Redirect: true, ACMEEmail: "ops@example.com", HubConfig: currentHubTOML(false)}, hubHost(), PanelAccessDeps{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	save := p.Steps[len(p.Steps)-1]
	if err := save.Apply(context.Background(), plan.NewEnv()); err != nil {
		t.Fatal(err)
	}
	stored, _ := cfg.Hub("hub-1")
	if stored.PanelMode() != config.PanelPublic || stored.PanelAccess.ACMEEmail != "ops@example.com" || strings.Join(stored.Cloud.Rules, ",") != "lazylongarm-hub-tcp,lazylongarm-hub-udp,lazylongarm-panel-https,lazylongarm-panel-http" {
		t.Fatalf("%+v %+v", stored.PanelAccess, stored.Cloud)
	}

	// hub setup rerun keeps the HTTPS panel in hub.toml and the profile.
	hp, err := BuildHubSetup(HubRequest{Host: "my-vps", Address: "hub.example.com", Name: "hub-1"}, hubHost(), HubDeps{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if out := render(hp); !strings.Contains(out, `https = ":443"`) || !strings.Contains(out, `acme_email = "ops@example.com"`) {
		t.Fatalf("hub setup dropped the HTTPS panel:\n%s", out)
	}
	env := plan.NewEnv()
	env.Set("ca_fingerprint", "sha256:aa")
	for _, s := range hp.Steps {
		if s.ID == "save-hub" {
			if err := s.Apply(context.Background(), env); err != nil {
				t.Fatal(err)
			}
		}
	}
	stored, _ = cfg.Hub("hub-1")
	if stored.PanelMode() != config.PanelPublic || stored.Cloud == nil || len(stored.Cloud.Rules) != 4 {
		t.Fatalf("save-hub lost panel access or cloud: %+v %+v", stored.PanelAccess, stored.Cloud)
	}

	// back to loopback clears it again.
	lp, err := BuildPanelAccess(PanelAccessRequest{Hub: stored, Mode: config.PanelLoopback, HubConfig: currentHubTOML(true)}, hubHost(), PanelAccessDeps{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if err := lp.Steps[len(lp.Steps)-1].Apply(context.Background(), plan.NewEnv()); err != nil {
		t.Fatal(err)
	}
	stored, _ = cfg.Hub("hub-1")
	if stored.PanelAccess != nil || strings.Join(stored.Cloud.Rules, ",") != "lazylongarm-hub-tcp,lazylongarm-hub-udp" {
		t.Fatalf("%+v %+v", stored.PanelAccess, stored.Cloud)
	}
}

func TestRestartHubOnlyWhenConfigChanged(t *testing.T) {
	s := restartHubStep("my-vps", "127.0.0.1:7480")
	env := plan.NewEnv()
	if skip, _ := s.Check(context.Background(), env); !skip {
		t.Fatal("restart without a config change")
	}
	env.Set(envHubConfigChanged, "1")
	if skip, _ := s.Check(context.Background(), env); skip {
		t.Fatal("config change did not restart")
	}
	env.Set(envHubStarted, "1")
	if skip, _ := s.Check(context.Background(), env); !skip {
		t.Fatal("a hub started in this run was restarted")
	}
}

func TestPanelAccessLoopbackOnLoopbackHubNeedsNoAzure(t *testing.T) {
	p, err := BuildPanelAccess(PanelAccessRequest{Hub: testHub(config.ModePublic, nil), Mode: config.PanelLoopback}, hubHost(), PanelAccessDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(p); got != "tailscale-serve-off,remove-bind-capability,restart-hub,save-panel-access" {
		t.Fatalf("steps %s", got)
	}
}
