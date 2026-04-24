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
