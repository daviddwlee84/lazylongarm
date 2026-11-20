package flows

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/release"
)

// InstallLocal downloads longarm + longarmd for this machine into
// ~/.local/bin (user-owned, so longarmd can self-update there). It returns
// the longarm path.
func InstallLocal(ctx context.Context, env *plan.Env, cacheDir, tag string) (string, error) {
	if tag == "" {
		var err error
		if tag, err = release.Latest(ctx); err != nil {
			return "", err
		}
	}
	env.Set("release_tag", tag)
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return "", err
	}
	var longarmPath string
	for _, component := range []string{"longarm", "longarmd"} {
		src, err := release.Fetch(ctx, cacheDir, tag, release.AssetName(component, runtime.GOOS, runtime.GOARCH), env.Logf)
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return "", err
		}
		dest := filepath.Join(bin, component)
		tmp := dest + ".lazylongarm.tmp"
		if err := os.WriteFile(tmp, data, 0o755); err != nil {
			return "", err
		}
		if err := os.Rename(tmp, dest); err != nil {
			os.Remove(tmp)
			return "", err
		}
		env.Logf("  %s → %s", filepath.Base(src), dest)
		if component == "longarm" {
			longarmPath = dest
		}
	}
	if longarmPath == "" {
		return "", fmt.Errorf("longarm was not installed")
	}
	return longarmPath, nil
}
