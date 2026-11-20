// Package skill embeds lazylongarm's agent operating guide so the binary and
// its instructions always match (`lazylongarm --skill`, `skill print`).
package skill

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed lazylongarm/SKILL.md
var files embed.FS

// Stub is the herdr-style SKILL.md that `lazylongarm skill sync` installs:
// the same frontmatter (so agents still trigger on it) and a body that sends
// them to `lazylongarm --skill`, which always matches the installed binary.
func Stub() (string, error) {
	full, err := Read("")
	if err != nil {
		return "", err
	}
	rest, ok := strings.CutPrefix(full, "---\n")
	if !ok {
		return "", fmt.Errorf("embedded SKILL.md has no frontmatter")
	}
	front, _, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return "", fmt.Errorf("embedded SKILL.md frontmatter is not closed")
	}
	return "---\n" + front + "\n---\n\n# lazylongarm\n\n" +
		"Installed by `lazylongarm skill sync`. The operating guide is embedded in the\n" +
		"binary, so it always matches the installed version. Read it before using\n" +
		"lazylongarm:\n\n    lazylongarm --skill\n", nil
}

// Read returns SKILL.md (topic "") or an unknown-topic error.
func Read(topic string) (string, error) {
	if topic == "" || topic == "skill" {
		b, err := files.ReadFile("lazylongarm/SKILL.md")
		return string(b), err
	}
	return "", fmt.Errorf("unknown skill topic %q (available: skill)", strings.TrimSpace(topic))
}
