package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/skill"
)

func TestSkillStubKeepsFrontmatterAndDefersToBinary(t *testing.T) {
	stub, err := skill.Stub()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stub, "---\nname: lazylongarm\ndescription:") || !strings.Contains(stub, "lazylongarm --skill") {
		t.Fatalf("stub:\n%s", stub)
	}
}

func TestSkillSyncStubAndClaudeLink(t *testing.T) {
	home := t.TempDir()
	p, err := buildSkillSync(home, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	env := plan.NewEnv()
	for _, s := range p.Steps {
		if s.ID != "lazylongarm-skill" && s.ID != "link-lazylongarm" {
			continue // the longarm steps need the real CLI
		}
		if ok, _ := s.Check(context.Background(), env); ok {
			t.Fatalf("%s satisfied before apply", s.ID)
		}
		if err := s.Apply(context.Background(), env); err != nil {
			t.Fatalf("%s: %v", s.ID, err)
		}
		if ok, _ := s.Check(context.Background(), env); !ok {
			t.Fatalf("%s not satisfied after apply", s.ID)
		}
	}
	link := filepath.Join(home, ".claude", "skills", "lazylongarm")
	if got, _ := os.Readlink(link); got != filepath.Join("..", "..", ".agents", "skills", "lazylongarm") {
		t.Fatalf("link -> %q", got)
	}
	if _, err := os.Stat(filepath.Join(link, "SKILL.md")); err != nil {
		t.Fatalf("link does not resolve: %v", err)
	}
	// A foreign directory is never replaced.
	other := filepath.Join(home, ".claude", "skills", "foreign")
	_ = os.MkdirAll(other, 0o755)
	if err := linkStep(filepath.Join(home, ".claude", "skills"), "foreign", nil).Apply(context.Background(), env); err == nil {
		t.Fatal("replaced an existing directory")
	}
}
