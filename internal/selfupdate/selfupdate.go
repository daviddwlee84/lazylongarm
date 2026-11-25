// Package selfupdate implements `lazylongarm upgrade`: decide who owns the
// running executable (Homebrew, a release archive, `go install`, or a local
// build), resolve one exact candidate tag, and either delegate to the owner or
// replace the resolved executable after verifying it.
package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/ghrelease"
)

// Repo and module coordinates.
const (
	Repo    = "daviddwlee84/lazylongarm"
	Module  = "github.com/daviddwlee84/lazylongarm"
	MainPkg = Module + "/cmd/lazylongarm"
	Binary  = "lazylongarm"
	Formula = "lazylongarm"
	// TapFormula is the formula's full name: brew commands use it, so a
	// same-named formula elsewhere (homebrew-core, another tap) is never
	// the one upgraded.
	TapFormula = "daviddwlee84/tap/" + Formula
	Checksums  = "checksums.txt"
)

// Channel is set by release builds: -X .../selfupdate.Channel=release-archive.
var Channel = ""

var (
	apiBase    = "https://api.github.com"
	dlBase     = "https://github.com"
	httpClient = &http.Client{Timeout: 5 * time.Minute}
	execCmd    = exec.CommandContext
	semverRE   = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)
)

// Owner of the running executable.
type Owner string

const (
	OwnerHomebrew  Owner = "homebrew"
	OwnerArchive   Owner = "release-archive"
	OwnerGoInstall Owner = "go-install"
	OwnerDev       Owner = "local-build"
)

// Check is the read-only upgrade assessment (`upgrade --check`).
type Check struct {
	Current  string `json:"current"`
	Latest   string `json:"latest,omitempty"`
	UpToDate bool   `json:"up_to_date"` // meaningful only with Latest; see MarshalJSON
	Exe      string `json:"executable"`
	Resolved string `json:"resolved"`
	Owner    Owner  `json:"owner"`
	// Formula is the tap-qualified formula of a Homebrew install.
	Formula   string   `json:"formula,omitempty"`
	Strategy  string   `json:"strategy"`
	Supported bool     `json:"apply_supported"`
	Notes     []string `json:"notes,omitempty"`
}

// MarshalJSON leaves out up_to_date when the latest release is unknown
// (rather than reporting a guessed false) and repeats apply_supported as
// "supported", the key package-manager checks (the Homebrew tap's) read.
func (c Check) MarshalJSON() ([]byte, error) {
	type plain Check
	out := struct {
		plain
		UpToDate  *bool `json:"up_to_date,omitempty"`
		Supported bool  `json:"supported"`
	}{plain: plain(c), Supported: c.Supported}
	if c.Latest != "" {
		v := c.UpToDate
		out.UpToDate = &v
	}
	return json.Marshal(out)
}

// Inspect resolves the owner and, unless the manager owns versioning, the
// latest published release.
func Inspect(ctx context.Context, current string) (Check, error) {
	c := Check{Current: current}
	exe, err := os.Executable()
	if err != nil {
		return c, err
	}
	c.Exe = exe
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		c.Resolved = r
	} else {
		c.Resolved = exe
	}
	info, _ := debug.ReadBuildInfo()
	modified := false
	mainVersion := ""
	if info != nil {
		mainVersion = info.Main.Version
		for _, s := range info.Settings {
			if s.Key == "vcs.modified" && s.Value == "true" {
				modified = true
			}
		}
	}
	switch {
	case strings.Contains(filepath.ToSlash(c.Resolved), "/Cellar/"+Formula+"/"):
		c.Owner, c.Supported, c.Formula = OwnerHomebrew, true, TapFormula
		c.Strategy = "brew update, then brew upgrade " + TapFormula + " (the tap can lag a fresh release)"
	case Channel == "release-archive" && !modified:
		c.Owner, c.Supported = OwnerArchive, true
		c.Strategy = "download the release archive for " + runtime.GOOS + "/" + runtime.GOARCH + ", verify " + Checksums + ", replace " + c.Resolved
	case semverRE.MatchString(mainVersion) && !modified:
		c.Owner = OwnerGoInstall
		if _, err := exec.LookPath("go"); err == nil {
			c.Supported = true
			c.Strategy = "GOBIN=<temp> go install " + MainPkg + "@<tag>, then replace " + c.Resolved
		} else {
			c.Strategy = "reinstall with: go install " + MainPkg + "@latest (Go is not on PATH)"
		}
	default:
		c.Owner = OwnerDev
		c.Strategy = "local or modified build: left alone; rebuild from source or install a release"
		c.Notes = append(c.Notes, "install a release with `go install "+MainPkg+"@latest` or from the GitHub release archives")
	}
	latest, err := LatestTag(ctx)
	if err != nil {
		if c.Owner == OwnerHomebrew {
			// brew knows what its tap offers; the version check after
			// the upgrade is skipped without a known latest release.
			c.Notes = append(c.Notes, "latest release unknown ("+err.Error()+"); Homebrew decides")
			return c, nil
		}
		return c, err
	}
	c.Latest = latest
	c.UpToDate = Compare(current, latest) >= 0
	return c, nil
}

// LatestTag returns the newest published (non-draft, non-prerelease) tag.
func LatestTag(ctx context.Context) (string, error) {
	tag, err := ghrelease.Latest(ctx, ghrelease.Source{Repo: Repo, APIBase: apiBase, WebBase: dlBase, UserAgent: "lazylongarm-upgrade", Client: httpClient, Valid: semverRE})
	if errors.Is(err, ghrelease.ErrNoRelease) {
		return "", errors.New("no published lazylongarm release yet")
	}
	return tag, err
}

// Compare orders vMAJOR.MINOR.PATCH tags; unparsable versions sort first.
func Compare(a, b string) int {
	pa, oka := parse(a)
	pb, okb := parse(b)
	switch {
	case !oka && !okb:
		return 0
	case !oka:
		return -1
	case !okb:
		return 1
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parse(v string) ([3]int, bool) {
	m := semverRE.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return [3]int{}, false
	}
	var out [3]int
	for i := 0; i < 3; i++ {
		out[i], _ = strconv.Atoi(m[i+1])
	}
	return out, true
}

// Apply performs the upgrade c describes. Progress goes to logw.
func Apply(ctx context.Context, c Check, logw io.Writer) error {
	if !c.Supported {
		return fmt.Errorf("upgrade is not supported for this installation: %s", c.Strategy)
	}
	switch c.Owner {
	case OwnerHomebrew:
		return applyHomebrew(ctx, c, logw)
	case OwnerArchive:
		return applyArchive(ctx, c, logw)
	case OwnerGoInstall:
		return applyGoInstall(ctx, c, logw)
	}
	return fmt.Errorf("no upgrade strategy for %s", c.Owner)
}

// applyHomebrew refreshes the taps first (brew upgrade skips its own
// auto-update for a while after the last one, and then only warns
// "already installed"), upgrades, and checks the version Homebrew ended
// up with: a tap that has not synced the release yet is an error, not a
// silent success.
func applyHomebrew(ctx context.Context, c Check, logw io.Writer) error {
	update := execCmd(ctx, "brew", "update", "--quiet")
	update.Stdout, update.Stderr = logw, logw
	if err := update.Run(); err != nil {
		fmt.Fprintf(logw, "brew update failed (%v); upgrading with the taps as they are\n", err)
	}
	up := execCmd(ctx, "brew", "upgrade", TapFormula)
	up.Stdout, up.Stderr = logw, logw
	if err := up.Run(); err != nil {
		return fmt.Errorf("brew upgrade %s: %w", TapFormula, err)
	}
	if c.Latest == "" {
		return nil
	}
	got, err := HomebrewVersion(ctx)
	if err != nil {
		return err
	}
	if Compare(got, c.Latest) < 0 {
		return fmt.Errorf("Homebrew still installs %s, the latest release is %s: the tap has not picked it up yet; try again later (or `brew update && brew upgrade %s`)", got, c.Latest, TapFormula)
	}
	return nil
}

// HomebrewVersion is the version `brew list --versions` reports for the
// formula, as "vX.Y.Z".
func HomebrewVersion(ctx context.Context) (string, error) {
	out, err := execCmd(ctx, "brew", "list", "--versions", TapFormula).Output()
	if err != nil {
		return "", fmt.Errorf("brew list --versions %s: %w", TapFormula, err)
	}
	f := strings.Fields(string(out))
	if len(f) < 2 {
		return "", fmt.Errorf("brew list --versions %s printed %q", TapFormula, strings.TrimSpace(string(out)))
	}
	return "v" + strings.TrimPrefix(f[len(f)-1], "v"), nil
}

func applyGoInstall(ctx context.Context, c Check, logw io.Writer) error {
	tmp, err := os.MkdirTemp("", "lazylongarm-upgrade-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	cmd := execCmd(ctx, "go", "install", MainPkg+"@"+c.Latest)
	cmd.Env = append(os.Environ(), "GOBIN="+tmp, "GOFLAGS=-mod=mod")
	cmd.Stdout, cmd.Stderr = logw, logw
	fmt.Fprintf(logw, "go install %s@%s (GOBIN=%s)\n", MainPkg, c.Latest, tmp)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go install: %w", err)
	}
	return replace(ctx, filepath.Join(tmp, Binary), c, logw)
}

func applyArchive(ctx context.Context, c Check, logw io.Writer) error {
	version := strings.TrimPrefix(c.Latest, "v")
	asset := fmt.Sprintf("%s_%s_%s_%s.tar.gz", Binary, version, runtime.GOOS, runtime.GOARCH)
	base := fmt.Sprintf("%s/%s/releases/download/%s/", dlBase, Repo, c.Latest)
	sums, err := fetch(ctx, base+Checksums, 1<<20)
	if err != nil {
		return err
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset {
			want = strings.ToLower(f[0])
		}
	}
	if want == "" {
		return fmt.Errorf("%s does not list %s", Checksums, asset)
	}
	fmt.Fprintf(logw, "downloading %s\n", asset)
	archive, err := fetch(ctx, base+asset, 200<<20)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(archive)
	if hex.EncodeToString(sum[:]) != want {
		return fmt.Errorf("checksum mismatch for %s", asset)
	}
	bin, err := extract(archive, Binary)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "lazylongarm-upgrade-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	staged := filepath.Join(tmp, Binary)
	if err := os.WriteFile(staged, bin, 0o755); err != nil {
		return err
	}
	return replace(ctx, staged, c, logw)
}

// replace verifies the staged binary reports the candidate version, then
// swaps it in next to the resolved executable with an atomic rename.
func replace(ctx context.Context, staged string, c Check, logw io.Writer) error {
	out, err := execCmd(ctx, staged, "version").Output()
	if err != nil {
		return fmt.Errorf("the new binary does not run: %w", err)
	}
	if !strings.Contains(string(out), c.Latest) {
		return fmt.Errorf("the new binary reports %q, expected %s", strings.TrimSpace(string(out)), c.Latest)
	}
	data, err := os.ReadFile(staged)
	if err != nil {
		return err
	}
	dir := filepath.Dir(c.Resolved)
	next := filepath.Join(dir, "."+Binary+".upgrade")
	if err := os.WriteFile(next, data, 0o755); err != nil {
		return fmt.Errorf("cannot write next to %s (%v); reinstall with the method you used", c.Resolved, err)
	}
	if err := os.Rename(next, c.Resolved); err != nil {
		os.Remove(next)
		return err
	}
	fmt.Fprintf(logw, "replaced %s with %s\n", c.Resolved, c.Latest)
	return nil
}

func fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "lazylongarm-upgrade")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("download %s: too large", url)
	}
	return b, nil
}

// extract returns the regular file named name from a tar.gz.
func extract(archive []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s not found in the archive", name)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == name && !strings.Contains(h.Name, "..") {
			return io.ReadAll(io.LimitReader(tr, 200<<20))
		}
	}
}
