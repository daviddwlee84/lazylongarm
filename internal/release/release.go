// Package release downloads Longarm release binaries from GitHub, verifies
// them against the release's SHA256SUMS, and caches them under
// $XDG_CACHE_HOME/lazylongarm/releases/<tag>/ for pushing to workers.
package release

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Repo is the upstream repository.
const Repo = "lubobill1990/longarm"

var (
	apiBase      = "https://api.github.com"
	downloadBase = "https://github.com"
	httpClient   = &http.Client{Timeout: 10 * time.Minute}
	tagRE        = regexp.MustCompile(`^v\d+\.\d+\.\d+([-.][0-9A-Za-z.]+)?$`)
)

// Latest returns the newest published release tag.
func Latest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/repos/"+Repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "lazylongarm")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("query latest Longarm release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("query latest Longarm release: HTTP %d", resp.StatusCode)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", err
	}
	if !tagRE.MatchString(body.TagName) {
		return "", fmt.Errorf("unexpected release tag %q", body.TagName)
	}
	return body.TagName, nil
}

// AssetName is the bare binary name for a component on a platform, e.g.
// longarmd-linux-amd64.
func AssetName(component, goos, goarch string) string {
	return component + "-" + goos + "-" + goarch
}

// Fetch returns the local path of a verified asset, downloading it into the
// cache when needed. A cached file is re-verified before use.
func Fetch(ctx context.Context, cacheDir, tag, asset string, logf func(string, ...any)) (string, error) {
	if !tagRE.MatchString(tag) {
		return "", fmt.Errorf("invalid release tag %q", tag)
	}
	if strings.ContainsAny(asset, "/\\") || asset == "" {
		return "", fmt.Errorf("invalid asset name %q", asset)
	}
	dir := filepath.Join(cacheDir, "releases", tag)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	sums, err := checksums(ctx, dir, tag)
	if err != nil {
		return "", err
	}
	want, ok := sums[asset]
	if !ok {
		return "", fmt.Errorf("%s is not listed in SHA256SUMS of %s", asset, tag)
	}
	path := filepath.Join(dir, asset)
	if got, err := fileSHA256(path); err == nil && got == want {
		return path, nil
	}
	if logf != nil {
		logf("  downloading %s %s from GitHub", asset, tag)
	}
	tmp := path + ".part"
	if err := download(ctx, fmt.Sprintf("%s/%s/releases/download/%s/%s", downloadBase, Repo, tag, asset), tmp, 200<<20); err != nil {
		return "", err
	}
	got, err := fileSHA256(tmp)
	if err != nil {
		return "", err
	}
	if got != want {
		os.Remove(tmp)
		return "", fmt.Errorf("checksum mismatch for %s %s: got %s, SHA256SUMS says %s", asset, tag, got, want)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	if logf != nil {
		logf("  verified %s sha256 %s…", asset, want[:16])
	}
	return path, nil
}

// SHA256 of a cached asset (for comparing with a remote copy).
func SHA256(path string) (string, error) { return fileSHA256(path) }

func checksums(ctx context.Context, dir, tag string) (map[string]string, error) {
	path := filepath.Join(dir, "SHA256SUMS")
	if _, err := os.Stat(path); err != nil {
		tmp := path + ".part"
		if err := download(ctx, fmt.Sprintf("%s/%s/releases/download/%s/SHA256SUMS", downloadBase, Repo, tag), tmp, 1<<20); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, path); err != nil {
			return nil, err
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 || len(fields[0]) != 64 {
			continue
		}
		out[strings.TrimPrefix(fields[1], "*")] = strings.ToLower(fields[0])
	}
	if len(out) == 0 {
		os.Remove(path)
		return nil, errors.New("SHA256SUMS is empty or malformed")
	}
	return out, sc.Err()
}

func download(ctx context.Context, url, dest string, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "lazylongarm")
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dest)
		return err
	}
	if n > limit {
		os.Remove(dest)
		return fmt.Errorf("download %s: larger than %d bytes", url, limit)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
