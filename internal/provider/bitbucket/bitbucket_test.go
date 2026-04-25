package bitbucket_test

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
	"github.com/Ogguz/gitraft/internal/provider/bitbucket"
)

func TestMatches(t *testing.T) {
	p := bitbucket.New(bitbucket.Options{})
	tests := []struct {
		raw  string
		want bool
	}{
		{"https://bitbucket.org/a/b.git", true},
		{"https://www.bitbucket.org/a/b.git", true},
		{"git@bitbucket.org:a/b.git", true},
		{"https://bitbucket.org:443/a/b.git", true},
		{"https://github.com/a/b.git", false},
		{"https://gitlab.com/a/b.git", false},
		{"https://my-bitbucket.example.com/a/b.git", false},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			u, err := provider.Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := p.Matches(u); got != tc.want {
				t.Errorf("Matches(%q) = %v; want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseRepo(t *testing.T) {
	p := bitbucket.New(bitbucket.Options{})
	u, _ := provider.Parse("https://bitbucket.org/myteam/coolproject.git")
	owner, name, err := p.ParseRepo(u)
	if err != nil {
		t.Fatal(err)
	}
	if owner != "myteam" || name != "coolproject" {
		t.Errorf("got (%q, %q)", owner, name)
	}
}

func TestAuthURL(t *testing.T) {
	t.Run("https with credentials embeds basic auth", func(t *testing.T) {
		p := bitbucket.New(bitbucket.Options{Username: "alice", AppPassword: "secret"})
		u, _ := provider.Parse("https://bitbucket.org/a/b.git")
		got, err := p.AuthURL(u)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "alice:secret@bitbucket.org") {
			t.Errorf("expected user:pass embedded; got %q", got)
		}
	})
	t.Run("both creds unset is unauthenticated passthrough", func(t *testing.T) {
		p := bitbucket.New(bitbucket.Options{}) // both empty — anonymous mode
		u, _ := provider.Parse("https://bitbucket.org/a/b.git")
		got, err := p.AuthURL(u)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "https://bitbucket.org/a/b.git" {
			t.Errorf("got %q; expected unchanged", got)
		}
	})
	t.Run("ssh passes through", func(t *testing.T) {
		p := bitbucket.New(bitbucket.Options{Username: "alice", AppPassword: "secret"})
		u, _ := provider.Parse("git@bitbucket.org:a/b.git")
		got, _ := p.AuthURL(u)
		if strings.Contains(got, "secret") {
			t.Errorf("token must not embed in SSH URL: %q", got)
		}
	})
	t.Run("existing userinfo is refused", func(t *testing.T) {
		p := bitbucket.New(bitbucket.Options{Username: "alice", AppPassword: "secret"})
		u, _ := url.Parse("https://existing:creds@bitbucket.org/a/b.git")
		_, err := p.AuthURL(u)
		if err == nil {
			t.Fatal("expected error for existing credentials")
		}
	})
}

func TestRepoExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repositories/team/exists":
			w.WriteHeader(http.StatusOK)
		case "/repositories/team/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/repositories/team/broken":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := bitbucket.New(bitbucket.Options{BaseURL: srv.URL, Username: "u", AppPassword: "p"})

	got, err := p.RepoExists(context.Background(), "team", "exists")
	if err != nil || !got {
		t.Errorf("exists: got (%v, %v)", got, err)
	}

	got, err = p.RepoExists(context.Background(), "team", "missing")
	if err != nil || got {
		t.Errorf("missing: got (%v, %v)", got, err)
	}

	_, err = p.RepoExists(context.Background(), "team", "broken")
	if err == nil {
		t.Error("expected error on 500")
	}
}

func TestRepoExists_BasicAuthHeader(t *testing.T) {
	var seenUser, seenPass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUser, seenPass, _ = r.BasicAuth()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := bitbucket.New(bitbucket.Options{BaseURL: srv.URL, Username: "alice", AppPassword: "appsecret"})
	if _, err := p.RepoExists(context.Background(), "a", "b"); err != nil {
		t.Fatal(err)
	}
	if seenUser != "alice" || seenPass != "appsecret" {
		t.Errorf("basic auth = (%q, %q); want (alice, appsecret)", seenUser, seenPass)
	}
}

func TestRepoExists_RefusesRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/repositories/new/location", http.StatusMovedPermanently)
	}))
	defer srv.Close()

	p := bitbucket.New(bitbucket.Options{BaseURL: srv.URL})
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
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := bitbucket.New(bitbucket.Options{BaseURL: srv.URL})
	_, err := p.RepoExists(context.Background(), "a", "b")
	if err == nil {
		t.Fatal("expected rate-limit error")
	}
	if !strings.Contains(err.Error(), "rate limited") || !strings.Contains(err.Error(), "120") {
		t.Errorf("expected rate-limit + Retry-After; got %v", err)
	}
}

func TestRepoExists_UnauthorizedWithoutCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"Unauthorized"}}`)
	}))
	defer srv.Close()

	p := bitbucket.New(bitbucket.Options{BaseURL: srv.URL}) // no creds
	_, err := p.RepoExists(context.Background(), "a", "b")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "BITBUCKET_USERNAME") {
		t.Errorf("expected credential hint; got %v", err)
	}
}

func TestCreateRepo_PostsToWorkspaceSlug(t *testing.T) {
	var seenPath string
	var seenBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"full_name":"team/new"}`)
	}))
	defer srv.Close()

	p := bitbucket.New(bitbucket.Options{BaseURL: srv.URL, Username: "u", AppPassword: "p"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{
		Owner:       "team",
		Name:        "new",
		Description: "hi",
		Visibility:  provider.VisibilityPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenPath != "/repositories/team/new" {
		t.Errorf("path = %q", seenPath)
	}
	if seenBody["scm"] != "git" {
		t.Errorf("scm = %v", seenBody["scm"])
	}
	if seenBody["is_private"] != true {
		t.Errorf("is_private = %v", seenBody["is_private"])
	}
	if seenBody["description"] != "hi" {
		t.Errorf("description = %v", seenBody["description"])
	}
}

func TestCreateRepo_PublicVisibility(t *testing.T) {
	var seenBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	p := bitbucket.New(bitbucket.Options{BaseURL: srv.URL, Username: "u", AppPassword: "p"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{
		Owner: "team", Name: "new", Visibility: provider.VisibilityPublic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenBody["is_private"] != false {
		t.Errorf("is_private should be false for public; got %v", seenBody["is_private"])
	}
}

func TestCreateRepo_AlreadyExists_400StringMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"Repository with this Slug and Owner already exists."}}`)
	}))
	defer srv.Close()

	p := bitbucket.New(bitbucket.Options{BaseURL: srv.URL, Username: "u", AppPassword: "p"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "team", Name: "dup"})
	if !errors.Is(err, provider.ErrRepoAlreadyExists) {
		t.Fatalf("expected ErrRepoAlreadyExists; got %v", err)
	}
}

func TestCreateRepo_AlreadyExists_400StructuredFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"validation failed","fields":{"name":["already exists"]}}}`)
	}))
	defer srv.Close()

	p := bitbucket.New(bitbucket.Options{BaseURL: srv.URL, Username: "u", AppPassword: "p"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "team", Name: "dup"})
	if !errors.Is(err, provider.ErrRepoAlreadyExists) {
		t.Fatalf("expected ErrRepoAlreadyExists; got %v", err)
	}
}

func TestCreateRepo_AlreadyExists_409(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":{"message":"already exists"}}`)
	}))
	defer srv.Close()

	p := bitbucket.New(bitbucket.Options{BaseURL: srv.URL, Username: "u", AppPassword: "p"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "team", Name: "dup"})
	if !errors.Is(err, provider.ErrRepoAlreadyExists) {
		t.Fatalf("expected ErrRepoAlreadyExists; got %v", err)
	}
}

func TestCreateRepo_BadRequest_NotAlreadyExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid scm"}}`)
	}))
	defer srv.Close()

	p := bitbucket.New(bitbucket.Options{BaseURL: srv.URL, Username: "u", AppPassword: "p"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "team", Name: "x"})
	if errors.Is(err, provider.ErrRepoAlreadyExists) {
		t.Fatalf("unrelated 400 must NOT be ErrRepoAlreadyExists")
	}
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestAuthURL_HalfSetCredentialsErrors(t *testing.T) {
	cases := []struct {
		name     string
		username string
		password string
	}{
		{"only username set", "alice", ""},
		{"only password set", "", "secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := bitbucket.New(bitbucket.Options{Username: tc.username, AppPassword: tc.password})
			u, _ := provider.Parse("https://bitbucket.org/a/b.git")
			_, err := p.AuthURL(u)
			if err == nil {
				t.Fatal("expected error for half-set credentials")
			}
			if !strings.Contains(err.Error(), "must be set together") {
				t.Errorf("expected 'must be set together' guidance; got %v", err)
			}
		})
	}
}

func TestAuthURL_SpecialCharactersInCredentials(t *testing.T) {
	username := "alice@example.com"
	password := "p@ss/word+:="
	p := bitbucket.New(bitbucket.Options{Username: username, AppPassword: password})
	u, _ := provider.Parse("https://bitbucket.org/a/b.git")
	got, err := p.AuthURL(u)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("result is not a valid URL: %v\n%s", err, got)
	}
	if parsed.User == nil {
		t.Fatal("expected userinfo in result")
	}
	if parsed.User.Username() != username {
		t.Errorf("username round-trip: got %q, want %q", parsed.User.Username(), username)
	}
	pw, _ := parsed.User.Password()
	if pw != password {
		t.Errorf("password round-trip: got %q, want %q", pw, password)
	}
}

func TestParseRepo_ScpLike(t *testing.T) {
	p := bitbucket.New(bitbucket.Options{})
	u, err := provider.Parse("git@bitbucket.org:myteam/coolproject.git")
	if err != nil {
		t.Fatal(err)
	}
	owner, name, err := p.ParseRepo(u)
	if err != nil {
		t.Fatal(err)
	}
	if owner != "myteam" || name != "coolproject" {
		t.Errorf("got (%q, %q); want (myteam, coolproject)", owner, name)
	}
}

func TestCreateRepo_VisibilityMappings(t *testing.T) {
	cases := []struct {
		name       string
		visibility provider.Visibility
		wantPriv   bool
	}{
		{"unspecified maps to private", provider.VisibilityUnspecified, true},
		{"private", provider.VisibilityPrivate, true},
		{"internal collapses to private", provider.VisibilityInternal, true},
		{"public", provider.VisibilityPublic, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seenBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&seenBody)
				w.WriteHeader(http.StatusCreated)
			}))
			defer srv.Close()

			p := bitbucket.New(bitbucket.Options{BaseURL: srv.URL, Username: "u", AppPassword: "p"})
			err := p.CreateRepo(context.Background(), provider.CreateOptions{
				Owner: "team", Name: "new", Visibility: tc.visibility,
			})
			if err != nil {
				t.Fatal(err)
			}
			if seenBody["is_private"] != tc.wantPriv {
				t.Errorf("is_private = %v; want %v", seenBody["is_private"], tc.wantPriv)
			}
		})
	}
}

func TestCreateRepo_AlreadyExists_400FieldsMultipleKeys(t *testing.T) {
	// Bitbucket's structured 400 may carry the "already exists" marker in any
	// of several fields — exercise multi-key shape.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"validation failed","fields":{"name":["taken"],"slug":["already exists"]}}}`)
	}))
	defer srv.Close()

	p := bitbucket.New(bitbucket.Options{BaseURL: srv.URL, Username: "u", AppPassword: "p"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "team", Name: "dup"})
	if !errors.Is(err, provider.ErrRepoAlreadyExists) {
		t.Fatalf("expected ErrRepoAlreadyExists from multi-key fields; got %v", err)
	}
}

func TestCreateRepo_NoFalsePositiveOnIncidentalAlreadyExistsText(t *testing.T) {
	// Body decodes as JSON but the structured paths don't carry the
	// already-exists marker — strict mode must NOT fall back to raw substring
	// match on the incidental phrase elsewhere in the body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid scm","fields":{"scm":["unsupported"]}},"hint":"see docs where this error already exists in our knowledge base"}`)
	}))
	defer srv.Close()

	p := bitbucket.New(bitbucket.Options{BaseURL: srv.URL, Username: "u", AppPassword: "p"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "team", Name: "x"})
	if errors.Is(err, provider.ErrRepoAlreadyExists) {
		t.Fatalf("incidental 'already exists' in unrelated body field must NOT trigger ErrRepoAlreadyExists; got: %v", err)
	}
	if err == nil {
		t.Fatal("expected generic 400 error")
	}
}
