package gitlab_test

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
	"github.com/Ogguz/gitraft/internal/provider/gitlab"
)

func TestMatches(t *testing.T) {
	tests := []struct {
		name string
		host string
		raw  string
		want bool
	}{
		{"default gitlab.com", "", "https://gitlab.com/a/b.git", true},
		{"default rejects self-hosted", "", "https://gitlab.example.com/a/b.git", false},
		{"self-hosted match", "gitlab.example.com", "https://gitlab.example.com/a/b.git", true},
		{"self-hosted rejects gitlab.com", "gitlab.example.com", "https://gitlab.com/a/b.git", false},
		{"scp-like nested", "", "git@gitlab.com:group/sub/proj.git", true},
		{"default rejects github", "", "https://github.com/a/b.git", false},
		{"port tolerated on URL", "", "https://gitlab.com:443/a/b.git", true},
		{"self-hosted tolerates port on URL", "gitlab.example.com", "https://gitlab.example.com:8080/a/b.git", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := gitlab.New(gitlab.Options{Host: tc.host})
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

func TestParseRepo(t *testing.T) {
	p := gitlab.New(gitlab.Options{})
	tests := []struct {
		raw       string
		wantOwner string
		wantName  string
		wantErr   bool
	}{
		{"https://gitlab.com/mygroup/myproj.git", "mygroup", "myproj", false},
		{"https://gitlab.com/mygroup/myproj", "mygroup", "myproj", false},
		{"https://gitlab.com/group/sub/proj.git", "group/sub", "proj", false},
		{"https://gitlab.com/a/b/c/d/proj.git", "a/b/c/d", "proj", false},
		{"git@gitlab.com:parent/child/grand/proj.git", "parent/child/grand", "proj", false},
		{"https://gitlab.com/onlyone", "", "", true},
		{"https://gitlab.com/group/.git", "", "", true}, // empty project name after .git strip
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			u, err := provider.Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			owner, name, err := p.ParseRepo(u)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				// Malformed-path errors must point the user at the expected
				// GitLab URL form (namespace/project, with subgroups allowed)
				// so they can self-correct without reading source. Anchored
				// on `\nhint:` (newline preamble) rather than bare `hint:`
				// because git itself emits `hint:` lines on stderr — without
				// the leading newline, an unrelated tail could satisfy this.
				if !strings.Contains(err.Error(), "\nhint:") {
					t.Errorf("ParseRepo error must include a `\\nhint:` preamble (newline-anchored); got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRepo: %v", err)
			}
			if owner != tc.wantOwner || name != tc.wantName {
				t.Errorf("got (%q, %q); want (%q, %q)", owner, name, tc.wantOwner, tc.wantName)
			}
		})
	}
}

func TestAuthURL(t *testing.T) {
	t.Run("https embeds oauth2 token", func(t *testing.T) {
		p := gitlab.New(gitlab.Options{Token: "secret"})
		u, _ := provider.Parse("https://gitlab.com/a/b.git")
		got, err := p.AuthURL(u)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "oauth2:secret@gitlab.com") {
			t.Errorf("expected oauth2:token embedded; got %q", got)
		}
	})
	t.Run("no token leaves URL alone", func(t *testing.T) {
		p := gitlab.New(gitlab.Options{})
		u, _ := provider.Parse("https://gitlab.com/a/b.git")
		got, _ := p.AuthURL(u)
		if got != "https://gitlab.com/a/b.git" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("ssh passes through", func(t *testing.T) {
		p := gitlab.New(gitlab.Options{Token: "secret"})
		u, _ := provider.Parse("git@gitlab.com:a/b.git")
		got, _ := p.AuthURL(u)
		if strings.Contains(got, "secret") {
			t.Errorf("token must not embed in SSH URL: %q", got)
		}
	})
	t.Run("existing userinfo is refused", func(t *testing.T) {
		p := gitlab.New(gitlab.Options{Token: "token"})
		u, _ := url.Parse("https://user:pass@gitlab.com/a/b.git")
		_, err := p.AuthURL(u)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestRepoExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/projects/group%2Fsub%2Fexists":
			w.WriteHeader(http.StatusOK)
		case "/projects/group%2Fsub%2Fmissing":
			w.WriteHeader(http.StatusNotFound)
		case "/projects/group%2Fsub%2Fbroken":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"message":"boom"}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.EscapedPath())
		}
	}))
	defer srv.Close()

	p := gitlab.New(gitlab.Options{BaseURL: srv.URL, Token: "t"})

	got, err := p.RepoExists(context.Background(), "group/sub", "exists")
	if err != nil || !got {
		t.Errorf("exists: got (%v, %v)", got, err)
	}

	got, err = p.RepoExists(context.Background(), "group/sub", "missing")
	if err != nil || got {
		t.Errorf("missing: got (%v, %v)", got, err)
	}

	_, err = p.RepoExists(context.Background(), "group/sub", "broken")
	if err == nil {
		t.Error("expected error on 500")
	}
}

func TestRepoExists_PrivateTokenHeader(t *testing.T) {
	var seenHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeader = r.Header.Get("PRIVATE-TOKEN")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := gitlab.New(gitlab.Options{BaseURL: srv.URL, Token: "mytoken"})
	_, err := p.RepoExists(context.Background(), "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if seenHeader != "mytoken" {
		t.Errorf("PRIVATE-TOKEN = %q, want %q", seenHeader, "mytoken")
	}
}

func TestRepoExists_RefusesRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/projects/new%2Flocation", http.StatusMovedPermanently)
	}))
	defer srv.Close()

	p := gitlab.New(gitlab.Options{BaseURL: srv.URL})
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
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := gitlab.New(gitlab.Options{BaseURL: srv.URL})
	_, err := p.RepoExists(context.Background(), "a", "b")
	if err == nil {
		t.Fatal("expected rate-limit error")
	}
	if !strings.Contains(err.Error(), "rate limited") || !strings.Contains(err.Error(), "30") {
		t.Errorf("expected rate-limit with Retry-After; got %v", err)
	}
	if !strings.Contains(err.Error(), "\nhint:") {
		t.Errorf("rate-limit error must include a `\\nhint:` preamble; got %v", err)
	}
}

func TestRepoExists_UnauthorizedWithoutToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"401 Unauthorized"}`)
	}))
	defer srv.Close()

	p := gitlab.New(gitlab.Options{BaseURL: srv.URL}) // no Token
	_, err := p.RepoExists(context.Background(), "a", "b")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "GITLAB_TOKEN is unset") {
		t.Errorf("expected helpful token hint; got %v", err)
	}
	// Lock the `\nhint:` preamble form so a regression to the old
	// inline-parenthetical style would fail. Without this, the
	// "GITLAB_TOKEN is unset" substring survives both formats.
	if !strings.Contains(err.Error(), "\nhint:") {
		t.Errorf("401 error must include a `\\nhint:` preamble; got %v", err)
	}
}

// TestRepoExists_UnauthorizedWithToken locks the bad/expired-token 401
// hint branch — distinct from the empty-token branch above. A regression
// that collapses the two would be caught here.
func TestRepoExists_UnauthorizedWithToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	p := gitlab.New(gitlab.Options{BaseURL: srv.URL, Token: "expired-or-revoked"})
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

func TestCreateRepo_LooksUpNamespaceAndPosts(t *testing.T) {
	var created map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.EscapedPath() == "/namespaces/mygroup%2Fsub":
			_, _ = io.WriteString(w, `{"id":42,"full_path":"mygroup/sub"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/projects":
			_ = json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":999}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
		}
	}))
	defer srv.Close()

	p := gitlab.New(gitlab.Options{BaseURL: srv.URL, Token: "t"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{
		Owner:       "mygroup/sub",
		Name:        "newproj",
		Description: "hi",
		Visibility:  provider.VisibilityPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created["name"] != "newproj" {
		t.Errorf("name = %v", created["name"])
	}
	if created["path"] != "newproj" {
		t.Errorf("path = %v", created["path"])
	}
	if created["namespace_id"] != float64(42) {
		t.Errorf("namespace_id = %v", created["namespace_id"])
	}
	if created["visibility"] != "private" {
		t.Errorf("visibility = %v", created["visibility"])
	}
	if created["description"] != "hi" {
		t.Errorf("description = %v", created["description"])
	}
}

func TestCreateRepo_NamespaceNotFound(t *testing.T) {
	var postAttempted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postAttempted = true
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	p := gitlab.New(gitlab.Options{BaseURL: srv.URL, Token: "t"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "nogroup", Name: "p"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "nogroup") || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected namespace-specific error naming the group; got %v", err)
	}
	// Namespace-not-found must hint at the most likely fix (token scope /
	// membership / rename) so users don't misdiagnose as a server bug. The
	// `\nhint:` prefix (rather than bare `hint:`) is checked because git
	// itself emits `hint:` lines on stderr — anchoring on the newline
	// preamble distinguishes our preamble from any inner-tail noise.
	if !strings.Contains(err.Error(), "\nhint:") {
		t.Errorf("namespace-not-found error must include a `\\nhint:` preamble; got %v", err)
	}
	if !strings.Contains(err.Error(), "GITLAB_TOKEN") {
		t.Errorf("hint should reference GITLAB_TOKEN scope/membership; got %v", err)
	}
	// `api` (write) scope is the actual minimum because namespaceID is only
	// invoked from CreateRepo, which then does POST /projects. Recommending
	// only `read_api` would mislead users into a second failure.
	if !strings.Contains(err.Error(), "`api` scope") {
		t.Errorf("hint should recommend `api` scope (write), not `read_api`; got %v", err)
	}
	if postAttempted {
		t.Error("POST /projects must not run when namespace lookup fails")
	}
}

func TestCreateRepo_NamespaceMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":`) // truncated
	}))
	defer srv.Close()

	p := gitlab.New(gitlab.Options{BaseURL: srv.URL, Token: "t"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "g", Name: "p"})
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("expected 'decode' in error; got %v", err)
	}
}

func TestCreateRepo_NamespaceZeroID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`) // valid JSON, but no id
	}))
	defer srv.Close()

	p := gitlab.New(gitlab.Options{BaseURL: srv.URL, Token: "t"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "g", Name: "p"})
	if err == nil {
		t.Fatal("expected error for missing id")
	}
	if !strings.Contains(err.Error(), "no id") {
		t.Errorf("expected 'no id' in error; got %v", err)
	}
}

func TestCreateRepo_AlreadyExists_409(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/namespaces/") {
			_, _ = io.WriteString(w, `{"id":1}`)
			return
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"message":"repo already exists"}`)
	}))
	defer srv.Close()

	p := gitlab.New(gitlab.Options{BaseURL: srv.URL, Token: "t"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "g", Name: "p"})
	if !errors.Is(err, provider.ErrRepoAlreadyExists) {
		t.Fatalf("expected ErrRepoAlreadyExists; got %v", err)
	}
}

func TestCreateRepo_AlreadyExists_BadRequest_StringMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/namespaces/") {
			_, _ = io.WriteString(w, `{"id":1}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"has already been taken"}`)
	}))
	defer srv.Close()

	p := gitlab.New(gitlab.Options{BaseURL: srv.URL, Token: "t"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "g", Name: "p"})
	if !errors.Is(err, provider.ErrRepoAlreadyExists) {
		t.Fatalf("expected ErrRepoAlreadyExists; got %v", err)
	}
}

func TestCreateRepo_AlreadyExists_BadRequest_StructuredMessage(t *testing.T) {
	// GitLab's real 400 response shape: {"message":{"name":["has already been taken"], "path":["..."]}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/namespaces/") {
			_, _ = io.WriteString(w, `{"id":1}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":{"name":["has already been taken"],"path":["has already been taken"]}}`)
	}))
	defer srv.Close()

	p := gitlab.New(gitlab.Options{BaseURL: srv.URL, Token: "t"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "g", Name: "p"})
	if !errors.Is(err, provider.ErrRepoAlreadyExists) {
		t.Fatalf("expected ErrRepoAlreadyExists; got %v", err)
	}
}

func TestCreateRepo_BadRequest_NotAlreadyExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/namespaces/") {
			_, _ = io.WriteString(w, `{"id":1}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"invalid project path"}`)
	}))
	defer srv.Close()

	p := gitlab.New(gitlab.Options{BaseURL: srv.URL, Token: "t"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "g", Name: "p"})
	if errors.Is(err, provider.ErrRepoAlreadyExists) {
		t.Fatalf("unrelated 400 should NOT be ErrRepoAlreadyExists")
	}
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSelfHostedBaseURL(t *testing.T) {
	p := gitlab.New(gitlab.Options{Host: "gitlab.example.com"})
	u, _ := provider.Parse("https://gitlab.example.com/a/b.git")
	if !p.Matches(u) {
		t.Errorf("self-hosted Matches should succeed for configured host")
	}
	uWrong, _ := provider.Parse("https://gitlab.com/a/b.git")
	if p.Matches(uWrong) {
		t.Errorf("self-hosted provider should not match gitlab.com")
	}
}
