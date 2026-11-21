package hostprobe

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const linuxOut = `os=Linux
arch=x86_64
distro_id=ubuntu
distro_pretty=Ubuntu 22.04.2 LTS
user=david
uid=1000
groups=david adm sudo docker
home=/home/david
sudo=nopasswd
has_systemctl=yes
has_apt-get=yes
svc_longarm_hub=inactive
mem_total_kb=1893040
mem_avail_kb=1109092
cpus=1
listen=tcp 0.0.0.0:443
listen=tcp [::]:22
listen=udp 127.0.0.53%lo:53
container=nginx
ver_longarmd=longarmd version v0.1.42 (abc)
probe_done=yes
`

func TestParseLinux(t *testing.T) {
	f, err := Parse(linuxOut)
	if err != nil {
		t.Fatal(err)
	}
	if f.OS != "linux" || f.Arch != "amd64" || f.Sudo != "nopasswd" || f.CPUs != 1 || !f.Has["apt-get"] {
		t.Fatalf("%+v", f)
	}
	if !f.PortBusy("tcp", 443) || !f.PortBusy("tcp", 22) || !f.PortBusy("udp", 53) || f.PortBusy("tcp", 7443) {
		t.Fatalf("ports %v", f.Ports())
	}
	if f.Versions["longarmd"] != "v0.1.42" {
		t.Fatalf("version %q", f.Versions["longarmd"])
	}
	if !strings.Contains(f.MemSummary(), "1.8 GB") {
		t.Fatalf("mem %s", f.MemSummary())
	}
}

func TestParseIncompleteProbe(t *testing.T) {
	if _, err := Parse("os=Linux\n"); err == nil {
		t.Fatal("expected incomplete probe error")
	}
}

func TestProbeScriptRunsLocally(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	f, err := Probe(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if f.OS == "" || f.Home == "" || f.User == "" {
		t.Fatalf("%+v", f)
	}
}

func TestRootEquivalent(t *testing.T) {
	f, err := Parse(linuxOut)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.RootEquivalent(), ", "); got != "group docker, passwordless sudo" {
		t.Fatalf("root-equivalent %q (groups %v)", got, f.Groups)
	}
	f.Groups, f.Sudo = []string{"staff"}, "password"
	if got := f.RootEquivalent(); len(got) != 0 {
		t.Fatalf("plain account flagged: %v", got)
	}
}

func TestClockSkewWarning(t *testing.T) {
	f, err := Parse("os=Darwin\narch=x86_64\nclock=1700000000\nprobe_done=yes\n")
	if err != nil || f.Clock != 1700000000 {
		t.Fatalf("%v %d", err, f.Clock)
	}
	f.Host = "mac-1"
	for _, tc := range []struct {
		skew time.Duration
		want string
	}{
		{90 * time.Second, ""},
		{-2*time.Hour - 51*time.Minute, "2h51m behind"},
		{10 * time.Minute, "10m ahead of"},
	} {
		f.ClockSkew = tc.skew
		got := f.ClockWarning()
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q", tc.skew, got)
		}
	}
	f.Clock = 0 // an older probe without the clock line
	if f.ClockWarning() != "" {
		t.Fatal("warned without a clock")
	}
}
