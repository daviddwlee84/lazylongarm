package ghrelease

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

func source(api, web string) Source {
	return Source{Repo: "o/r", APIBase: api, WebBase: web, UserAgent: "t", Client: http.DefaultClient, Valid: regexp.MustCompile(`^v\d+\.\d+\.\d+$`)}
}

func TestLatestAPI(t *testing.T) {
	t.Setenv("GH_TOKEN", "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("no token: %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"tag_name":"v1.2.3"}`))
	}))
	defer srv.Close()
	if tag, err := Latest(context.Background(), source(srv.URL, "http://unused.invalid")); err != nil || tag != "v1.2.3" {
		t.Fatalf("%q %v", tag, err)
	}
}

func TestLatestFallsBackToWebOnRateLimit(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer api.Close()
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/o/r/releases/latest" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/o/r/releases/tag/v2.0.1", http.StatusFound)
	}))
	defer web.Close()
	if tag, err := Latest(context.Background(), source(api.URL, web.URL)); err != nil || tag != "v2.0.1" {
		t.Fatalf("%q %v", tag, err)
	}
	// Both down: the error names the rate limit.
	if _, err := Latest(context.Background(), source(api.URL, "http://127.0.0.1:1")); err == nil || !regexp.MustCompile(`rate limit`).MatchString(err.Error()) {
		t.Fatalf("%v", err)
	}
}

func TestLatestNoRelease(t *testing.T) {
	api := httptest.NewServer(http.NotFoundHandler())
	defer api.Close()
	if _, err := Latest(context.Background(), source(api.URL, "http://unused.invalid")); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("%v", err)
	}
}
