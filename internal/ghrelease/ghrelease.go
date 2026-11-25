// Package ghrelease finds a GitHub repository's latest published release
// tag. The REST API allows 60 unauthenticated requests an hour per IP, which
// a shared proxy or NAT exhausts quickly, so a token from GH_TOKEN /
// GITHUB_TOKEN is used when set, and a failed API call falls back to the
// web redirect of /<repo>/releases/latest, which has no such limit.
package ghrelease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// ErrNoRelease means the repository has no published release.
var ErrNoRelease = errors.New("no published release")

// Source says where to look.
type Source struct {
	Repo      string // owner/name
	APIBase   string // https://api.github.com
	WebBase   string // https://github.com
	UserAgent string
	Client    *http.Client
	Valid     *regexp.Regexp // accepted tag form
}

// Latest returns the newest published (non-draft, non-prerelease) tag.
func Latest(ctx context.Context, s Source) (string, error) {
	tag, apiErr := s.api(ctx)
	if apiErr == nil || errors.Is(apiErr, ErrNoRelease) {
		return tag, apiErr
	}
	tag, webErr := s.web(ctx)
	if webErr == nil {
		return tag, nil
	}
	return "", fmt.Errorf("%w (and %s/%s/releases/latest: %v)", apiErr, s.WebBase, s.Repo, webErr)
}

func token() string {
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func (s Source) api(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.APIBase+"/repos/"+s.Repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", s.UserAgent)
	if t := token(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("query the latest %s release: %w", s.Repo, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", ErrNoRelease
	case resp.StatusCode != http.StatusOK:
		msg := fmt.Sprintf("query the latest %s release: HTTP %d", s.Repo, resp.StatusCode)
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			msg += " (GitHub API rate limit; set GH_TOKEN to raise it)"
		}
		return "", errors.New(msg)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", err
	}
	return s.check(body.TagName)
}

// web reads the tag from the redirect github.com sends for
// /<repo>/releases/latest (…/releases/tag/<tag>), without following it.
func (s Source) web(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, s.WebBase+"/"+s.Repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", s.UserAgent)
	client := *s.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode < 300 || resp.StatusCode > 399 || loc == "" {
		return "", fmt.Errorf("HTTP %d without a redirect", resp.StatusCode)
	}
	i := strings.LastIndex(loc, "/releases/tag/")
	if i < 0 {
		return "", ErrNoRelease // redirected to /releases: none published
	}
	return s.check(loc[i+len("/releases/tag/"):])
}

func (s Source) check(tag string) (string, error) {
	if s.Valid != nil && !s.Valid.MatchString(tag) {
		return "", fmt.Errorf("unexpected release tag %q", tag)
	}
	return tag, nil
}
