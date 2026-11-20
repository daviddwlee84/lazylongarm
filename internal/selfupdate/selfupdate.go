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
)

// Repo and module coordinates.
const (
	Repo      = "daviddwlee84/lazylongarm"
	Module    = "github.com/daviddwlee84/lazylongarm"
	MainPkg   = Module + "/cmd/lazylongarm"
	Binary    = "lazylongarm"
	Formula   = "lazylongarm"
	Checksums = "checksums.txt"
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
	Current   string   `json:"current"`
	Latest    string   `json:"latest,omitempty"`
	UpToDate  bool     `json:"up_to_date"`
	Exe       string   `json:"executable"`
	Resolved  string   `json:"resolved"`
	Owner     Owner    `json:"owner"`
	Strategy  string   `json:"strategy"`
	Supported bool     `json:"apply_supported"`
	Notes     []string `json:"notes,omitempty"`
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
		c.Owner, c.Supported = OwnerHomebrew, true
		c.Strategy = "brew upgrade " + Formula + "   (Homebrew decides the available version)"
		return c, nil
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
		return c, err
	}
	c.Latest = latest
	c.UpToDate = Compare(current, latest) >= 0
	return c, nil
}

// LatestTag returns the newest published (non-draft, non-prerelease) tag.
func LatestTag(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/repos/"+Repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "lazylongarm-upgrade")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("query the latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", errors.New("no published lazylongarm release yet")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("query the latest release: HTTP %d", resp.StatusCode)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", err
	}
	if !semverRE.MatchString(body.TagName) {
		return "", fmt.Errorf("unexpected release tag %q", body.TagName)
	}
	return body.TagName, nil
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
		cmd := execCmd(ctx, "brew", "upgrade", Formula)
		cmd.Stdout, cmd.Stderr = logw, logw
		return cmd.Run()
	case OwnerArchive:
		return applyArchive(ctx, c, logw)
	case OwnerGoInstall:
		return applyGoInstall(ctx, c, logw)
	}
	return fmt.Errorf("no upgrade strategy for %s", c.Owner)
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
