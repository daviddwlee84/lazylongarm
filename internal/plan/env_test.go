package plan

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/secret"
)

// Regression: Check steps rely on errors.As(*CommandError) to tell "not in
// place yet" from a real failure; redaction must not erase the error type.
func TestEnvRunKeepsErrorTypes(t *testing.T) {
	env := NewEnv()
	for name, run := range map[string]func(context.Context, connection.Request) (connection.Result, error){
		"Run": env.Run, "RunQuiet": env.RunQuiet,
	} {
		_, err := run(context.Background(), connection.Request{Script: `test -s /nonexistent/lazylongarm`})
		var ce *connection.CommandError
		if !errors.As(err, &ce) || ce.ExitCode != 1 {
			t.Fatalf("%s: error type lost: %#v", name, err)
		}
	}
}

func TestEnvRunRedactsButUnwraps(t *testing.T) {
	env := NewEnv()
	env.SetSecret("code", secret.New("TOPSECRET-42"))
	_, err := env.RunQuiet(context.Background(), connection.Request{Script: `echo "bad code TOPSECRET-42" >&2; exit 3`})
	if err == nil || strings.Contains(err.Error(), "TOPSECRET-42") {
		t.Fatalf("not redacted: %v", err)
	}
	var ce *connection.CommandError
	if !errors.As(err, &ce) || ce.ExitCode != 3 {
		t.Fatalf("cause not reachable: %#v", err)
	}
}
