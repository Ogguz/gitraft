package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Ogguz/gitraft/internal/provider"
	"github.com/Ogguz/gitraft/internal/provider/github"
)

func TestMatches(t *testing.T) {
	p := github.New(github.Options{})
	tests := []struct {
		raw  string
		want bool
	}{
		{"https://github.com/a/b.git", true},
		{"git@github.com:a/b.git", true},
		{"https://www.github.com/a/b.git", true},
		{"https://github.com:443/a/b.git", true},
		{"https://gitlab.com/a/b.git", false},
		{"https://example.com/a/b.git", false},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			u, err := provider.Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := p.Matches(u); got != tc.want {
				t.Errorf("Matches(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestAuthURL(t *testing.T) {
	t.Run("https with token embeds token", func(t *testing.T) {
		p := github.New(github.Options{Token: "secret"})
		u, _ := provider.Parse("https://github.com/a/b.git")
		got, err := p.AuthURL(u)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "x-access-token:secret@github.com") {
			t.Errorf("expected token embedded; got %q", got)
		}
	})
	t.Run("https without token is unchanged", func(t *testing.T) {
		p := github.New(github.Options{})
		u, _ := provider.Parse("https://github.com/a/b.git")
		got, _ := p.AuthURL(u)
		if got != "https://github.com/a/b.git" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("ssh passes through", func(t *testing.T) {
		p := github.New(github.Options{Token: "secret"})
		u, _ := provider.Parse("git@github.com:a/b.git")
		got, _ := p.AuthURL(u)
		if strings.Contains(got, "secret") {
			t.Errorf("token must not embed in SSH URL: %q", got)
		}
	})
	t.Run("existing userinfo is refused", func(t *testing.T) {
		p := github.New(github.Options{Token: "token"})
		u, _ := url.Parse("https://user:pass@github.com/a/b.git")
		_, err := p.AuthURL(u)
		if err == nil {
			t.Fatal("expected error for URL with existing credentials")
		}
	})
}

func TestRepoExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/exists":
			w.WriteHeader(http.StatusOK)
		case "/repos/owner/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/repos/owner/broken":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"message":"boom"}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := github.New(github.Options{BaseURL: srv.URL, Token: "t"})

	got, err := p.RepoExists(context.Background(), "owner", "exists")
	if err != nil || !got {
		t.Errorf("exists: got (%v, %v)", got, err)
	}

	got, err = p.RepoExists(context.Background(), "owner", "missing")
	if err != nil || got {
		t.Errorf("missing: got (%v, %v)", got, err)
	}

	_, err = p.RepoExists(context.Background(), "owner", "broken")
	if err == nil {
		t.Error("expected error on 500")
	}
}

func TestRepoExists_RefusesRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/repos/newowner/newname", http.StatusMovedPermanently)
	}))
	defer srv.Close()

	p := github.New(github.Options{BaseURL: srv.URL, Token: "t"})
	_, err := p.RepoExists(context.Background(), "a", "b")
	if err == nil {
		t.Fatal("expected redirect refusal")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("expected 'redirect' in error; got %v", err)
	}
	// Redirect-refusal must point the user at the most likely fix
	// (rename/move at the source) so they don't misdiagnose it as a
	// gitraft bug. Anchored on `\nhint:` (newline preamble).
	if !strings.Contains(err.Error(), "\nhint:") {
		t.Errorf("redirect-refusal error must include a `\\nhint:` preamble (newline-anchored); got %v", err)
	}
}

func TestRepoExists_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := github.New(github.Options{BaseURL: srv.URL, Token: "t"})
	_, err := p.RepoExists(context.Background(), "a", "b")
	if err == nil {
		t.Fatal("expected rate-limit error")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("expected 'rate limited' in error; got %v", err)
	}
	if !strings.Contains(err.Error(), "60") {
		t.Errorf("expected Retry-After seconds in error; got %v", err)
	}
	if !strings.Contains(err.Error(), "\nhint:") {
		t.Errorf("rate-limit error must include a `\\nhint:` preamble; got %v", err)
	}
}

// TestRepoExists_UnauthorizedWithoutToken locks the empty-token 401 hint
// branch (added when github apiError was converted from a free function
// to a method). A regression that drops the empty-token branch would
// either fall through to the catch-all wrap (no hint) or emit the
// token-set hint (wrong remediation).
func TestRepoExists_UnauthorizedWithoutToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	p := github.New(github.Options{BaseURL: srv.URL}) // no token
	_, err := p.RepoExists(context.Background(), "a", "b")
	if err == nil {
		t.Fatal("expected 401 error")
	}
	if !strings.Contains(err.Error(), "401 Unauthorized") {
		t.Errorf("expected '401 Unauthorized' in error; got %v", err)
	}
	if !strings.Contains(err.Error(), "\nhint:") {
		t.Errorf("401 error must include a `\\nhint:` preamble; got %v", err)
	}
	if !strings.Contains(err.Error(), "GITHUB_TOKEN unset") {
		t.Errorf("empty-token 401 hint should mention `GITHUB_TOKEN unset`; got %v", err)
	}
	if !strings.Contains(err.Error(), "repo") {
		t.Errorf("hint should reference `repo` scope; got %v", err)
	}
}

// TestRepoExists_UnauthorizedWithToken locks the bad/expired-token 401
// hint branch — distinct from the empty-token branch above, so a
// regression that collapses the two would be caught here.
func TestRepoExists_UnauthorizedWithToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	p := github.New(github.Options{BaseURL: srv.URL, Token: "expired-or-revoked"})
	_, err := p.RepoExists(context.Background(), "a", "b")
	if err == nil {
		t.Fatal("expected 401 error")
	}
	if !strings.Contains(err.Error(), "401 Unauthorized") {
		t.Errorf("expected '401 Unauthorized' in error; got %v", err)
	}
	if !strings.Contains(err.Error(), "\nhint:") {
		t.Errorf("401 error must include a `\\nhint:` preamble; got %v", err)
	}
	if !strings.Contains(err.Error(), "expired") || !strings.Contains(err.Error(), "revoked") {
		t.Errorf("token-set 401 hint should mention `expired or revoked`; got %v", err)
	}
}

func TestCreateRepo_OrganizationPath(t *testing.T) {
	var captured struct {
		path string
		body map[string]any
		auth string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/acme":
			_, _ = io.WriteString(w, `{"type":"Organization"}`)
		case "/orgs/acme/repos":
			captured.path = r.URL.Path
			captured.auth = r.Header.Get("Authorization")
			_ = json.NewDecoder(r.Body).Decode(&captured.body)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"full_name":"acme/new"}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := github.New(github.Options{BaseURL: srv.URL, Token: "mytoken"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{
		Owner: "acme", Name: "new",
		Description: "hi", Visibility: provider.VisibilityPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if captured.path != "/orgs/acme/repos" {
		t.Errorf("path = %q", captured.path)
	}
	if captured.body["name"] != "new" || captured.body["description"] != "hi" || captured.body["private"] != true {
		t.Errorf("body = %+v", captured.body)
	}
	if captured.auth != "Bearer mytoken" {
		t.Errorf("auth header = %q", captured.auth)
	}
}

func TestCreateRepo_UserPath(t *testing.T) {
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/alice":
			_, _ = io.WriteString(w, `{"type":"User"}`)
		case "/user":
			_, _ = io.WriteString(w, `{"login":"alice"}`)
		case "/user/repos":
			captured = r.URL.Path
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := github.New(github.Options{BaseURL: srv.URL, Token: "t"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{
		Owner: "alice", Name: "new",
	})
	if err != nil {
		t.Fatal(err)
	}
	if captured != "/user/repos" {
		t.Errorf("path = %q", captured)
	}
}

func TestCreateRepo_UserMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/alice":
			_, _ = io.WriteString(w, `{"type":"User"}`)
		case "/user":
			_, _ = io.WriteString(w, `{"login":"bob"}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := github.New(github.Options{BaseURL: srv.URL, Token: "t"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{
		Owner: "alice", Name: "new",
	})
	if err == nil {
		t.Fatal("expected error for user mismatch")
	}
	if !strings.Contains(err.Error(), "alice") || !strings.Contains(err.Error(), "bob") {
		t.Errorf("error should name both logins; got: %v", err)
	}
}

func TestCreateRepo_AlreadyExists_422(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/acme":
			_, _ = io.WriteString(w, `{"type":"Organization"}`)
		case "/orgs/acme/repos":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"message":"Repository creation failed.","errors":[{"message":"name already exists on this account"}]}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := github.New(github.Options{BaseURL: srv.URL, Token: "t"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "acme", Name: "new"})
	if !errors.Is(err, provider.ErrRepoAlreadyExists) {
		t.Fatalf("expected ErrRepoAlreadyExists; got %v", err)
	}
}

func TestCreateRepo_AlreadyExists_409(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/acme":
			_, _ = io.WriteString(w, `{"type":"Organization"}`)
		case "/orgs/acme/repos":
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"message":"repo already exists"}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := github.New(github.Options{BaseURL: srv.URL, Token: "t"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "acme", Name: "new"})
	if !errors.Is(err, provider.ErrRepoAlreadyExists) {
		t.Fatalf("expected ErrRepoAlreadyExists; got %v", err)
	}
}

func TestCreateRepo_422_NotAlreadyExists(t *testing.T) {
	// 422 for a different validation reason should NOT be treated as already-exists.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/acme":
			_, _ = io.WriteString(w, `{"type":"Organization"}`)
		case "/orgs/acme/repos":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"message":"invalid name"}`)
		}
	}))
	defer srv.Close()

	p := github.New(github.Options{BaseURL: srv.URL, Token: "t"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "acme", Name: "new"})
	if errors.Is(err, provider.ErrRepoAlreadyExists) {
		t.Fatalf("expected generic 422 error; got ErrRepoAlreadyExists")
	}
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParseRepo(t *testing.T) {
	p := github.New(github.Options{})
	u, _ := url.Parse("https://github.com/octocat/hello-world.git")
	owner, name, err := p.ParseRepo(u)
	if err != nil {
		t.Fatal(err)
	}
	if owner != "octocat" || name != "hello-world" {
		t.Errorf("got (%q, %q)", owner, name)
	}
}

// ---- GitHub Enterprise Server (GHE) routing ----
//
// Verifies that setting Options.Host engages GHE mode: Matches routes by
// the configured hostname (no implicit www. variant), and the default API
// base URL is derived as https://<host>/api/v3 rather than the SaaS
// api.github.com endpoint. This is the contract --github-url depends on.

// TestMatches_GHEHost asserts the routing rules differ from SaaS mode:
// the configured host is the only one that matches, github.com itself
// does NOT match a GHE-configured provider.
func TestMatches_GHEHost(t *testing.T) {
	p := github.New(github.Options{Host: "github.example.com"})
	tests := []struct {
		raw  string
		want bool
	}{
		{"https://github.example.com/a/b.git", true},
		{"git@github.example.com:a/b.git", true},
		{"https://github.example.com:443/a/b.git", true},
		// Unlike SaaS mode, www. is not implicitly accepted — GHE installs
		// typically have one canonical hostname.
		{"https://www.github.example.com/a/b.git", false},
		// SaaS github.com must NOT match a GHE-configured provider, or
		// users with both flags would route GHE traffic through SaaS auth.
		{"https://github.com/a/b.git", false},
		{"https://gitlab.com/a/b.git", false},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			u, err := provider.Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := p.Matches(u); got != tc.want {
				t.Errorf("Matches(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestNew_GHEDerivesApiV3BaseURL verifies the default API endpoint logic:
// when Host is set and BaseURL is empty, the provider derives
// https://<host>/api/v3 (the GHE convention) and routes API calls
// through it. Tested via a recording RoundTripper rather than an
// httptest server because we want to observe the URL the provider
// *attempted* to reach, not just whether a stub server responded.
func TestNew_GHEDerivesApiV3BaseURL(t *testing.T) {
	rt := &recordingRoundTripper{}
	p := github.New(github.Options{
		Host:       "github.example.com",
		Token:      "t",
		HTTPClient: &http.Client{Transport: rt},
	})
	// Trigger any API call; we're not checking response handling, only
	// the URL the provider built.
	_, _ = p.RepoExists(context.Background(), "owner", "repo")

	if rt.lastURL == "" {
		t.Fatal("provider did not make an HTTP request")
	}
	const wantPrefix = "https://github.example.com/api/v3/repos/owner/repo"
	if !strings.HasPrefix(rt.lastURL, wantPrefix) {
		t.Errorf("expected URL prefix %q; got %q", wantPrefix, rt.lastURL)
	}
}

// TestNew_BaseURLOverrideWinsOverHost verifies that an explicit BaseURL
// overrides the host-based derivation — important so tests (and any
// future proxy use case) can point the client at an httptest server
// without having to also fake a hostname.
func TestNew_BaseURLOverrideWinsOverHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := github.New(github.Options{
		Host:    "github.example.com", // would derive https://github.example.com/api/v3
		BaseURL: srv.URL,              // explicit override
		Token:   "t",
	})
	// If BaseURL didn't win, this call would fail with DNS / connection
	// error against github.example.com (which doesn't resolve).
	exists, err := p.RepoExists(context.Background(), "owner", "repo")
	if err != nil {
		t.Fatalf("RepoExists: %v", err)
	}
	if !exists {
		t.Error("expected exists=true from httptest 200 response")
	}
}

// TestNew_DefaultsPreserveSaaSBehavior is the regression guard: with no
// Host set, the provider must keep hitting api.github.com so existing
// non-GHE deployments aren't broken by the GHE plumbing.
func TestNew_DefaultsPreserveSaaSBehavior(t *testing.T) {
	rt := &recordingRoundTripper{}
	p := github.New(github.Options{
		Token:      "t",
		HTTPClient: &http.Client{Transport: rt},
	})
	_, _ = p.RepoExists(context.Background(), "owner", "repo")
	const wantPrefix = "https://api.github.com/repos/owner/repo"
	if !strings.HasPrefix(rt.lastURL, wantPrefix) {
		t.Errorf("expected SaaS URL prefix %q; got %q", wantPrefix, rt.lastURL)
	}
}

// recordingRoundTripper captures the URL of the last request the client
// attempted, then returns a minimal 200 OK response. Used by GHE / SaaS
// derivation tests where we want to observe the URL the provider built
// without standing up an httptest server (a server's URL would mask the
// derivation logic the test is verifying).
type recordingRoundTripper struct {
	lastURL string
}

func (r *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.lastURL = req.URL.String()
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("{}")),
		Header:     make(http.Header),
	}, nil
}
