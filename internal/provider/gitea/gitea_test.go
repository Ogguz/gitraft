package gitea_test

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
	"github.com/Ogguz/gitraft/internal/provider/gitea"
)

func TestMatches_RequiresHostConfigured(t *testing.T) {
	t.Run("unconfigured matches nothing", func(t *testing.T) {
		p := gitea.New(gitea.Options{})
		u, _ := provider.Parse("https://gitea.example.com/a/b.git")
		if p.Matches(u) {
			t.Error("unconfigured provider must not match any URL")
		}
		uCom, _ := provider.Parse("https://gitea.com/a/b.git")
		if p.Matches(uCom) {
			t.Error("unconfigured provider must not match gitea.com (no default)")
		}
	})
	t.Run("configured matches exact host", func(t *testing.T) {
		p := gitea.New(gitea.Options{Host: "gitea.example.com"})
		u, _ := provider.Parse("https://gitea.example.com/a/b.git")
		if !p.Matches(u) {
			t.Error("configured provider should match its host")
		}
	})
	t.Run("configured rejects mismatched host", func(t *testing.T) {
		p := gitea.New(gitea.Options{Host: "gitea.example.com"})
		u, _ := provider.Parse("https://gitea.com/a/b.git")
		if p.Matches(u) {
			t.Error("configured provider should not match different host")
		}
	})
	t.Run("port stripped", func(t *testing.T) {
		p := gitea.New(gitea.Options{Host: "gitea.example.com"})
		u, _ := provider.Parse("https://gitea.example.com:8443/a/b.git")
		if !p.Matches(u) {
			t.Error("Matches should ignore the URL port")
		}
	})
	t.Run("scp-like ssh", func(t *testing.T) {
		p := gitea.New(gitea.Options{Host: "gitea.example.com"})
		u, _ := provider.Parse("git@gitea.example.com:a/b.git")
		if !p.Matches(u) {
			t.Error("scp-like ssh URL should match configured host")
		}
	})
}

func TestParseRepo(t *testing.T) {
	p := gitea.New(gitea.Options{Host: "gitea.example.com"})
	u, _ := provider.Parse("https://gitea.example.com/myteam/coolproject.git")
	owner, name, err := p.ParseRepo(u)
	if err != nil {
		t.Fatal(err)
	}
	if owner != "myteam" || name != "coolproject" {
		t.Errorf("got (%q, %q)", owner, name)
	}
}

func TestAuthURL(t *testing.T) {
	t.Run("https with token embeds token-as-username", func(t *testing.T) {
		p := gitea.New(gitea.Options{Host: "gitea.example.com", Token: "mytok"})
		u, _ := provider.Parse("https://gitea.example.com/a/b.git")
		got, err := p.AuthURL(u)
		if err != nil {
			t.Fatal(err)
		}
		// Gitea: token embedded as URL username, no password.
		if !strings.Contains(got, "mytok@gitea.example.com") {
			t.Errorf("expected token-as-username embed; got %q", got)
		}
		// Make sure there's no colon (which would indicate username:password form).
		if strings.Contains(got, "mytok:") {
			t.Errorf("token must not appear in user:password form; got %q", got)
		}
	})
	t.Run("no token leaves URL alone", func(t *testing.T) {
		p := gitea.New(gitea.Options{Host: "gitea.example.com"})
		u, _ := provider.Parse("https://gitea.example.com/a/b.git")
		got, err := p.AuthURL(u)
		if err != nil {
			t.Fatal(err)
		}
		if got != "https://gitea.example.com/a/b.git" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("ssh passes through", func(t *testing.T) {
		p := gitea.New(gitea.Options{Host: "gitea.example.com", Token: "mytok"})
		u, _ := provider.Parse("git@gitea.example.com:a/b.git")
		got, _ := p.AuthURL(u)
		if strings.Contains(got, "mytok") {
			t.Errorf("token must not embed in SSH URL: %q", got)
		}
	})
	t.Run("existing userinfo is refused", func(t *testing.T) {
		p := gitea.New(gitea.Options{Host: "gitea.example.com", Token: "mytok"})
		u, _ := url.Parse("https://existing:creds@gitea.example.com/a/b.git")
		_, err := p.AuthURL(u)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestRepoExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/team/exists":
			w.WriteHeader(http.StatusOK)
		case "/repos/team/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/repos/team/broken":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"message":"boom"}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := gitea.New(gitea.Options{BaseURL: srv.URL, Token: "tok"})

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

func TestRepoExists_TokenAuthHeader(t *testing.T) {
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := gitea.New(gitea.Options{BaseURL: srv.URL, Token: "mytok"})
	if _, err := p.RepoExists(context.Background(), "a", "b"); err != nil {
		t.Fatal(err)
	}
	if seenAuth != "token mytok" {
		t.Errorf("Authorization = %q; want %q", seenAuth, "token mytok")
	}
}

func TestRepoExists_RefusesRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/repos/new/loc", http.StatusMovedPermanently)
	}))
	defer srv.Close()

	p := gitea.New(gitea.Options{BaseURL: srv.URL})
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
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := gitea.New(gitea.Options{BaseURL: srv.URL})
	_, err := p.RepoExists(context.Background(), "a", "b")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "rate limited") || !strings.Contains(err.Error(), "45") {
		t.Errorf("expected rate-limit + Retry-After; got %v", err)
	}
}

func TestRepoExists_UnauthorizedWithoutToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"Unauthorized"}`)
	}))
	defer srv.Close()

	p := gitea.New(gitea.Options{BaseURL: srv.URL}) // no token
	_, err := p.RepoExists(context.Background(), "a", "b")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "GITEA_TOKEN unset") {
		t.Errorf("expected GITEA_TOKEN-unset hint; got %v", err)
	}
}

func TestRepoExists_UnauthorizedWithToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"Unauthorized"}`)
	}))
	defer srv.Close()

	p := gitea.New(gitea.Options{BaseURL: srv.URL, Token: "wrong"})
	_, err := p.RepoExists(context.Background(), "a", "b")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "expired") && !strings.Contains(err.Error(), "revoked") {
		t.Errorf("expected token validity hint; got %v", err)
	}
}

func TestRepoExists_NotConfiguredErrors(t *testing.T) {
	p := gitea.New(gitea.Options{}) // no host, no BaseURL
	_, err := p.RepoExists(context.Background(), "a", "b")
	if err == nil {
		t.Fatal("expected error for unconfigured provider")
	}
	if !strings.Contains(err.Error(), "--gitea-url") {
		t.Errorf("expected '--gitea-url' hint; got %v", err)
	}
}

func TestCreateRepo_NotConfiguredErrors(t *testing.T) {
	p := gitea.New(gitea.Options{})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "o", Name: "r"})
	if err == nil {
		t.Fatal("expected error for unconfigured provider")
	}
	if !strings.Contains(err.Error(), "--gitea-url") {
		t.Errorf("expected '--gitea-url' hint; got %v", err)
	}
}

func TestCreateRepo_OrgPath(t *testing.T) {
	var seenPath string
	var seenBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/acme":
			_, _ = io.WriteString(w, `{"id":7,"username":"acme"}`)
		case "/orgs/acme/repos":
			seenPath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&seenBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":99}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := gitea.New(gitea.Options{BaseURL: srv.URL, Token: "tok"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{
		Owner: "acme", Name: "newrepo", Description: "hi",
		Visibility: provider.VisibilityPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenPath != "/orgs/acme/repos" {
		t.Errorf("path = %q", seenPath)
	}
	if seenBody["name"] != "newrepo" {
		t.Errorf("name = %v", seenBody["name"])
	}
	if seenBody["private"] != true {
		t.Errorf("private = %v", seenBody["private"])
	}
	if seenBody["description"] != "hi" {
		t.Errorf("description = %v", seenBody["description"])
	}
	if seenBody["auto_init"] != false {
		t.Errorf("auto_init must be explicit false; got %v", seenBody["auto_init"])
	}
}

func TestCreateRepo_OrgZeroIDRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /orgs/acme returns 200 but body lacks id — defensive check fires.
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	p := gitea.New(gitea.Options{BaseURL: srv.URL, Token: "tok"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "acme", Name: "r"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "without an organization id") {
		t.Errorf("expected id-validation error; got %v", err)
	}
}

func TestCreateRepo_OrgDetectionErrorWrapped(t *testing.T) {
	// /orgs/acme returns 401 (e.g., private org token can't read) — the error
	// must surface that we couldn't determine the type, not just a bare 401.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"Unauthorized"}`)
	}))
	defer srv.Close()

	p := gitea.New(gitea.Options{BaseURL: srv.URL, Token: "wrong"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "acme", Name: "r"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "cannot determine if") {
		t.Errorf("expected type-detection wrap; got %v", err)
	}
}

func TestCreateRepo_UserPath(t *testing.T) {
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/alice":
			w.WriteHeader(http.StatusNotFound)
		case "/user":
			_, _ = io.WriteString(w, `{"login":"alice"}`)
		case "/user/repos":
			seenPath = r.URL.Path
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := gitea.New(gitea.Options{BaseURL: srv.URL, Token: "tok"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{
		Owner: "alice", Name: "newrepo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenPath != "/user/repos" {
		t.Errorf("path = %q", seenPath)
	}
}

func TestCreateRepo_UserMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/alice":
			w.WriteHeader(http.StatusNotFound)
		case "/user":
			_, _ = io.WriteString(w, `{"login":"bob"}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := gitea.New(gitea.Options{BaseURL: srv.URL, Token: "tok"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{
		Owner: "alice", Name: "r",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "alice") || !strings.Contains(err.Error(), "bob") {
		t.Errorf("error should name both logins; got: %v", err)
	}
}

func TestCreateRepo_AuthLoginFailure(t *testing.T) {
	// /orgs/alice returns 404 (user, not org), but the /user fetch fails.
	// CreateRepo should bubble up the /user error with the credential hint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/alice":
			w.WriteHeader(http.StatusNotFound)
		case "/user":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"message":"Unauthorized"}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := gitea.New(gitea.Options{BaseURL: srv.URL}) // no token — /user 401s
	err := p.CreateRepo(context.Background(), provider.CreateOptions{
		Owner: "alice", Name: "r",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "GITEA_TOKEN") {
		t.Errorf("expected credential hint to surface from /user failure; got: %v", err)
	}
}

func TestCreateRepo_PublicVisibility(t *testing.T) {
	var seenBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/acme":
			_, _ = io.WriteString(w, `{"id":1}`)
		case "/orgs/acme/repos":
			_ = json.NewDecoder(r.Body).Decode(&seenBody)
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer srv.Close()

	p := gitea.New(gitea.Options{BaseURL: srv.URL, Token: "tok"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{
		Owner: "acme", Name: "r", Visibility: provider.VisibilityPublic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenBody["private"] != false {
		t.Errorf("private should be false for public; got %v", seenBody["private"])
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
				switch r.URL.Path {
				case "/orgs/acme":
					_, _ = io.WriteString(w, `{"id":1}`)
				case "/orgs/acme/repos":
					_ = json.NewDecoder(r.Body).Decode(&seenBody)
					w.WriteHeader(http.StatusCreated)
				}
			}))
			defer srv.Close()

			p := gitea.New(gitea.Options{BaseURL: srv.URL, Token: "tok"})
			err := p.CreateRepo(context.Background(), provider.CreateOptions{
				Owner: "acme", Name: "r", Visibility: tc.visibility,
			})
			if err != nil {
				t.Fatal(err)
			}
			if seenBody["private"] != tc.wantPriv {
				t.Errorf("private = %v; want %v", seenBody["private"], tc.wantPriv)
			}
		})
	}
}

func TestCreateRepo_DescriptionTrimmedOmitWhenEmpty(t *testing.T) {
	var seenBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/acme":
			_, _ = io.WriteString(w, `{"id":1}`)
		case "/orgs/acme/repos":
			_ = json.NewDecoder(r.Body).Decode(&seenBody)
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer srv.Close()

	p := gitea.New(gitea.Options{BaseURL: srv.URL, Token: "tok"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{
		Owner: "acme", Name: "r", Description: "  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := seenBody["description"]; ok {
		t.Errorf("whitespace-only description should be omitted; got %v", seenBody["description"])
	}
}

func TestCreateRepo_AlreadyExists_422(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/acme":
			_, _ = io.WriteString(w, `{"id":1}`)
		case "/orgs/acme/repos":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"message":"The repository with the same name already exists."}`)
		}
	}))
	defer srv.Close()

	p := gitea.New(gitea.Options{BaseURL: srv.URL, Token: "tok"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "acme", Name: "dup"})
	if !errors.Is(err, provider.ErrRepoAlreadyExists) {
		t.Fatalf("expected ErrRepoAlreadyExists; got %v", err)
	}
}

func TestCreateRepo_AlreadyExists_409(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/acme":
			_, _ = io.WriteString(w, `{"id":1}`)
		case "/orgs/acme/repos":
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"message":"repo already exists"}`)
		}
	}))
	defer srv.Close()

	p := gitea.New(gitea.Options{BaseURL: srv.URL, Token: "tok"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "acme", Name: "dup"})
	if !errors.Is(err, provider.ErrRepoAlreadyExists) {
		t.Fatalf("expected ErrRepoAlreadyExists; got %v", err)
	}
}

func TestCreateRepo_RawFallbackOnNonJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/acme":
			_, _ = io.WriteString(w, `{"id":1}`)
		case "/orgs/acme/repos":
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `<html>Repository already exists</html>`)
		}
	}))
	defer srv.Close()

	p := gitea.New(gitea.Options{BaseURL: srv.URL, Token: "tok"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "acme", Name: "dup"})
	if !errors.Is(err, provider.ErrRepoAlreadyExists) {
		t.Fatalf("expected raw fallback to detect already-exists; got %v", err)
	}
}

func TestCreateRepo_BadRequest_NotAlreadyExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/acme":
			_, _ = io.WriteString(w, `{"id":1}`)
		case "/orgs/acme/repos":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"message":"invalid name"}`)
		}
	}))
	defer srv.Close()

	p := gitea.New(gitea.Options{BaseURL: srv.URL, Token: "tok"})
	err := p.CreateRepo(context.Background(), provider.CreateOptions{Owner: "acme", Name: "x"})
	if errors.Is(err, provider.ErrRepoAlreadyExists) {
		t.Fatal("unrelated 422 must NOT be ErrRepoAlreadyExists")
	}
	if err == nil {
		t.Fatal("expected error")
	}
}
