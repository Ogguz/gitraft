package bitbucketserver_test

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
	"github.com/Ogguz/gitraft/internal/provider/bitbucketserver"
)

func TestMatches_RequiresHostConfigured(t *testing.T) {
	t.Run("unconfigured matches nothing", func(t *testing.T) {
		p := bitbucketserver.New(bitbucketserver.Options{})
		u, _ := provider.Parse("https://bitbucket.example.com/scm/proj/repo.git")
		if p.Matches(u) {
			t.Error("unconfigured provider must not match any URL")
		}
	})
	t.Run("configured matches exact host", func(t *testing.T) {
		p := bitbucketserver.New(bitbucketserver.Options{Host: "bitbucket.example.com"})
		u, _ := provider.Parse("https://bitbucket.example.com/scm/proj/repo.git")
		if !p.Matches(u) {
			t.Error("configured provider should match its host")
		}
	})
	t.Run("configured rejects mismatched host", func(t *testing.T) {
		p := bitbucketserver.New(bitbucketserver.Options{Host: "bitbucket.example.com"})
		u, _ := provider.Parse("https://bitbucket.org/scm/proj/repo.git")
		if p.Matches(u) {
			t.Error("configured provider should not match a different host")
		}
	})
	t.Run("port stripped from URL host", func(t *testing.T) {
		p := bitbucketserver.New(bitbucketserver.Options{Host: "bitbucket.example.com"})
		u, _ := provider.Parse("https://bitbucket.example.com:8443/scm/proj/repo.git")
		if !p.Matches(u) {
			t.Error("Matches should ignore the URL port")
		}
	})
}

func TestParseRepo(t *testing.T) {
	p := bitbucketserver.New(bitbucketserver.Options{Host: "bb.example.com"})
	tests := []struct {
		name        string
		raw         string
		wantProject string
		wantRepo    string
		wantErr     bool
	}{
		{"https clone /scm/", "https://bb.example.com/scm/PROJ/myrepo.git", "PROJ", "myrepo", false},
		{"browser /projects/.../repos/", "https://bb.example.com/projects/PROJ/repos/myrepo", "PROJ", "myrepo", false},
		{"https with context path", "https://bb.example.com/bitbucket/scm/PROJ/myrepo.git", "PROJ", "myrepo", false},
		{"browser with context path", "https://bb.example.com/bitbucket/projects/PROJ/repos/myrepo", "PROJ", "myrepo", false},
		{"ssh scp-like", "git@bb.example.com:PROJ/myrepo.git", "PROJ", "myrepo", false},
		{"ssh with port", "ssh://git@bb.example.com:7999/PROJ/myrepo.git", "PROJ", "myrepo", false},
		{"personal repo /scm/~user/", "https://bb.example.com/scm/~alice/myrepo.git", "~alice", "myrepo", false},
		{"personal repo browser", "https://bb.example.com/projects/~alice/repos/myrepo", "~alice", "myrepo", false},
		{"personal repo SSH scp-like", "git@bb.example.com:~alice/myrepo.git", "~alice", "myrepo", false},
		{"missing repo segment", "https://bb.example.com/scm/PROJ", "", "", true},
		{"only host no path", "https://bb.example.com/", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := provider.Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			project, repo, err := p.ParseRepo(u)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRepo: %v", err)
			}
			if project != tc.wantProject || repo != tc.wantRepo {
				t.Errorf("got (%q, %q); want (%q, %q)", project, repo, tc.wantProject, tc.wantRepo)
			}
		})
	}
}

func TestAuthURL(t *testing.T) {
	t.Run("https with creds embeds basic auth", func(t *testing.T) {
		p := bitbucketserver.New(bitbucketserver.Options{
			Host: "bb.example.com", Username: "alice", Token: "tok",
		})
		u, _ := provider.Parse("https://bb.example.com/scm/proj/repo.git")
		got, err := p.AuthURL(u)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "alice:tok@bb.example.com") {
			t.Errorf("expected user:tok embedded; got %q", got)
		}
	})
	t.Run("both creds unset is unauthenticated passthrough", func(t *testing.T) {
		p := bitbucketserver.New(bitbucketserver.Options{Host: "bb.example.com"})
		u, _ := provider.Parse("https://bb.example.com/scm/proj/repo.git")
		got, err := p.AuthURL(u)
		if err != nil {
			t.Fatal(err)
		}
		if got != "https://bb.example.com/scm/proj/repo.git" {
			t.Errorf("got %q; expected unchanged", got)
		}
	})
	t.Run("ssh passes through", func(t *testing.T) {
		p := bitbucketserver.New(bitbucketserver.Options{
			Host: "bb.example.com", Username: "alice", Token: "tok",
		})
		u, _ := provider.Parse("ssh://git@bb.example.com:7999/proj/repo.git")
		got, _ := p.AuthURL(u)
		if strings.Contains(got, "tok") {
			t.Errorf("token must not embed in SSH URL: %q", got)
		}
	})
	t.Run("existing userinfo is refused", func(t *testing.T) {
		p := bitbucketserver.New(bitbucketserver.Options{
			Host: "bb.example.com", Username: "alice", Token: "tok",
		})
		u, _ := url.Parse("https://existing:creds@bb.example.com/scm/proj/repo.git")
		_, err := p.AuthURL(u)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestAuthURL_HalfSetCredentialsErrors(t *testing.T) {
	cases := []struct {
		name     string
		username string
		token    string
	}{
		{"only username set", "alice", ""},
		{"only token set", "", "tok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := bitbucketserver.New(bitbucketserver.Options{
				Host: "bb.example.com", Username: tc.username, Token: tc.token,
			})
			u, _ := provider.Parse("https://bb.example.com/scm/proj/repo.git")
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

func TestRepoExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/projects/PROJ/repos/exists":
			w.WriteHeader(http.StatusOK)
		case "/projects/PROJ/repos/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/projects/PROJ/repos/broken":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"errors":[{"message":"boom"}]}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := bitbucketserver.New(bitbucketserver.Options{
		BaseURL: srv.URL, Username: "u", Token: "t",
	})

	got, err := p.RepoExists(context.Background(), "PROJ", "exists")
	if err != nil || !got {
		t.Errorf("exists: got (%v, %v)", got, err)
	}

	got, err = p.RepoExists(context.Background(), "PROJ", "missing")
	if err != nil || got {
		t.Errorf("missing: got (%v, %v)", got, err)
	}

	_, err = p.RepoExists(context.Background(), "PROJ", "broken")
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

	p := bitbucketserver.New(bitbucketserver.Options{
		BaseURL: srv.URL, Username: "alice", Token: "mytok",
	})
	if _, err := p.RepoExists(context.Background(), "PROJ", "myrepo"); err != nil {
		t.Fatal(err)
	}
	if seenUser != "alice" || seenPass != "mytok" {
		t.Errorf("basic auth = (%q, %q); want (alice, mytok)", seenUser, seenPass)
	}
}

func TestRepoExists_RefusesRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/projects/NEW/repos/loc", http.StatusMovedPermanently)
	}))
	defer srv.Close()

	p := bitbucketserver.New(bitbucketserver.Options{BaseURL: srv.URL})
	_, err := p.RepoExists(context.Background(), "PROJ", "repo")
	if err == nil {
		t.Fatal("expected redirect refusal")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("expected 'redirect' in error; got %v", err)
	}
}

func TestRepoExists_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "90")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := bitbucketserver.New(bitbucketserver.Options{BaseURL: srv.URL})
	_, err := p.RepoExists(context.Background(), "PROJ", "repo")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "rate limited") || !strings.Contains(err.Error(), "90") {
		t.Errorf("expected rate-limit + Retry-After; got %v", err)
	}
}

func TestRepoExists_UnauthorizedWithoutCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"errors":[{"message":"Unauthorized"}]}`)
	}))
	defer srv.Close()

	p := bitbucketserver.New(bitbucketserver.Options{BaseURL: srv.URL})
	_, err := p.RepoExists(context.Background(), "PROJ", "repo")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "BITBUCKET_SERVER_USERNAME") {
		t.Errorf("expected credential hint; got %v", err)
	}
}

func TestCreateRepo_PostsToProject(t *testing.T) {
	var seenPath string
	var seenBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"slug":"newrepo"}`)
	}))
	defer srv.Close()

	p := bitbucketserver.New(bitbucketserver.Options{BaseURL: srv.URL, Username: "u", Token: "t"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{
		Owner:       "PROJ",
		Name:        "newrepo",
		Description: "hi",
		Visibility:  provider.VisibilityPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenPath != "/projects/PROJ/repos" {
		t.Errorf("path = %q", seenPath)
	}
	if seenBody["name"] != "newrepo" {
		t.Errorf("name = %v", seenBody["name"])
	}
	if seenBody["scmId"] != "git" {
		t.Errorf("scmId = %v", seenBody["scmId"])
	}
	if seenBody["forkable"] != true {
		t.Errorf("forkable = %v", seenBody["forkable"])
	}
	if seenBody["public"] != false {
		t.Errorf("public should be false for private; got %v", seenBody["public"])
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

	p := bitbucketserver.New(bitbucketserver.Options{BaseURL: srv.URL, Username: "u", Token: "t"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{
		Owner: "PROJ", Name: "r", Visibility: provider.VisibilityPublic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenBody["public"] != true {
		t.Errorf("public should be true for VisibilityPublic; got %v", seenBody["public"])
	}
}

func TestCreateRepo_AlreadyExists_409Structured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"errors":[{"message":"Repository already exists","exceptionName":"com.atlassian.bitbucket.repository.RepositorySlugTakenException"}]}`)
	}))
	defer srv.Close()

	p := bitbucketserver.New(bitbucketserver.Options{BaseURL: srv.URL, Username: "u", Token: "t"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "PROJ", Name: "r"})
	if !errors.Is(err, provider.ErrRepoAlreadyExists) {
		t.Fatalf("expected ErrRepoAlreadyExists; got %v", err)
	}
}

func TestCreateRepo_AlreadyExists_ExceptionName(t *testing.T) {
	// Message doesn't contain "already exists" but exceptionName has "Taken"
	// — exception-name detection still triggers ErrRepoAlreadyExists.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"errors":[{"message":"slug conflict","exceptionName":"com.atlassian.bitbucket.repository.RepositorySlugTakenException"}]}`)
	}))
	defer srv.Close()

	p := bitbucketserver.New(bitbucketserver.Options{BaseURL: srv.URL, Username: "u", Token: "t"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "PROJ", Name: "r"})
	if !errors.Is(err, provider.ErrRepoAlreadyExists) {
		t.Fatalf("expected ErrRepoAlreadyExists from exceptionName; got %v", err)
	}
}

func TestCreateRepo_BadRequest_NotAlreadyExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"errors":[{"message":"invalid scmId","exceptionName":"com.atlassian.bitbucket.ValidationException"}]}`)
	}))
	defer srv.Close()

	p := bitbucketserver.New(bitbucketserver.Options{BaseURL: srv.URL, Username: "u", Token: "t"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "PROJ", Name: "r"})
	if errors.Is(err, provider.ErrRepoAlreadyExists) {
		t.Fatal("unrelated 400 must NOT be ErrRepoAlreadyExists")
	}
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestCreateRepo_UnrelatedTakenExceptionIsNotAlreadyExists(t *testing.T) {
	// Regression: substring match on "Taken" used to misclassify generic
	// lock/resource exceptions as ErrRepoAlreadyExists.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"errors":[{"message":"resource is locked","exceptionName":"com.atlassian.bitbucket.lock.ResourceLockTakenException"}]}`)
	}))
	defer srv.Close()

	p := bitbucketserver.New(bitbucketserver.Options{BaseURL: srv.URL, Username: "u", Token: "t"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "PROJ", Name: "r"})
	if errors.Is(err, provider.ErrRepoAlreadyExists) {
		t.Fatal("ResourceLockTakenException must NOT be classified as ErrRepoAlreadyExists")
	}
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestCreateRepo_RawFallbackOnNonJSONBody(t *testing.T) {
	// Some proxies and WAFs replace API error bodies with HTML pages. The raw
	// substring fallback is the last line of defense for already-exists detection.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `<html><body>Repository already exists</body></html>`)
	}))
	defer srv.Close()

	p := bitbucketserver.New(bitbucketserver.Options{BaseURL: srv.URL, Username: "u", Token: "t"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "PROJ", Name: "r"})
	if !errors.Is(err, provider.ErrRepoAlreadyExists) {
		t.Fatalf("expected raw fallback to detect already-exists; got %v", err)
	}
}

func TestCreateRepo_DescriptionOmittedWhenEmpty(t *testing.T) {
	var seenBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	p := bitbucketserver.New(bitbucketserver.Options{BaseURL: srv.URL, Username: "u", Token: "t"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "PROJ", Name: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := seenBody["description"]; ok {
		t.Errorf("empty description should be omitted from body; got %v", seenBody["description"])
	}
}

func TestCreateRepo_DescriptionTrimsWhitespace(t *testing.T) {
	var seenBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	p := bitbucketserver.New(bitbucketserver.Options{BaseURL: srv.URL, Username: "u", Token: "t"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{
		Owner: "PROJ", Name: "r", Description: "   ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := seenBody["description"]; ok {
		t.Errorf("whitespace-only description should be omitted; got %v", seenBody["description"])
	}
}

func TestRepoExists_NotConfiguredErrors(t *testing.T) {
	p := bitbucketserver.New(bitbucketserver.Options{}) // both Host and BaseURL empty
	_, err := p.RepoExists(context.Background(), "PROJ", "repo")
	if err == nil {
		t.Fatal("expected error for unconfigured provider")
	}
	if !strings.Contains(err.Error(), "--bitbucket-url") {
		t.Errorf("expected '--bitbucket-url' hint; got %v", err)
	}
}

func TestCreateRepo_NotConfiguredErrors(t *testing.T) {
	p := bitbucketserver.New(bitbucketserver.Options{})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "PROJ", Name: "r"})
	if err == nil {
		t.Fatal("expected error for unconfigured provider")
	}
	if !strings.Contains(err.Error(), "--bitbucket-url") {
		t.Errorf("expected '--bitbucket-url' hint; got %v", err)
	}
}

func TestRepoExists_UnauthorizedWithCredentials(t *testing.T) {
	// Set creds but server rejects them — different message vs the unset case.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"errors":[{"message":"Unauthorized"}]}`)
	}))
	defer srv.Close()

	p := bitbucketserver.New(bitbucketserver.Options{BaseURL: srv.URL, Username: "u", Token: "wrong"})
	_, err := p.RepoExists(context.Background(), "PROJ", "repo")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "expired") && !strings.Contains(err.Error(), "revoked") {
		t.Errorf("expected hint about token validity (expired/revoked); got %v", err)
	}
}
