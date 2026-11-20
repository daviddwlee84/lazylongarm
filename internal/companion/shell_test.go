package companion

import (
	"os/exec"
	"testing"
)

func TestScriptsParseWithSh(t *testing.T) {
	boot, err := BootstrapScript("", map[string]string{"repo": "you"}, []string{"--promptString", "name=Jane Doe"})
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]string{"state": StateScript, "bootstrap": boot, "update": UpdateScript(false)} {
		if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("%s: %v %s", name, err, out)
		}
	}
}

func TestStateScriptRunsLocally(t *testing.T) {
	out, err := exec.Command("sh", "-c", StateScript).Output()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseState(string(out)); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
