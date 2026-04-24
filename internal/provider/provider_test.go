package provider_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/Ogguz/gitraft/internal/provider"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantHost string
		wantPath string
		wantErr  bool
	}{
		{"https", "https://github.com/a/b.git", "github.com", "/a/b.git", false},
		{"scp-like", "git@github.com:a/b.git", "github.com", "/a/b.git", false},
		{"ssh", "ssh://git@github.com/a/b.git", "github.com", "/a/b.git", false},
		{"empty", "", "", "", true},
		{"no host", "just-a-path", "", "", true},
		{"scp-like nested", "git@gitlab.example.com:group/sub/repo.git", "gitlab.example.com", "/group/sub/repo.git", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := provider.Parse(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", u)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if u.Host != tc.wantHost {
				t.Errorf("host = %q, want %q", u.Host, tc.wantHost)
			}
			if u.Path != tc.wantPath {
				t.Errorf("path = %q, want %q", u.Path, tc.wantPath)
			}
		})
	}
}

func TestSplitPath(t *testing.T) {
	tests := []struct {
		raw       string
		wantOwner string
		wantName  string
		wantErr   bool
	}{
		{"https://github.com/owner/repo.git", "owner", "repo", false},
		{"https://github.com/owner/repo", "owner", "repo", false},
		{"https://github.com/onlyowner", "", "", true},
		{"https://github.com/a/b/c", "", "", true}, // nested paths rejected; provider must implement ParseRepo
		{"https://github.com/a/b/c/d/file.go", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			u, err := provider.Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			owner, name, err := provider.SplitPath(u)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("SplitPath: %v", err)
			}
			if owner != tc.wantOwner || name != tc.wantName {
				t.Errorf("got (%q, %q), want (%q, %q)", owner, name, tc.wantOwner, tc.wantName)
			}
		})
	}
}

func TestDetect(t *testing.T) {
	gh := &fakeProvider{name: "github", hosts: []string{"github.com"}}
	gl := &fakeProvider{name: "gitlab", hosts: []string{"gitlab.com"}}
	ps := []provider.Provider{gh, gl}

	tests := []struct {
		raw  string
		want string
	}{
		{"https://github.com/a/b.git", "github"},
		{"git@gitlab.com:a/b.git", "gitlab"},
		{"https://bitbucket.org/a/b.git", ""},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			u, err := provider.Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			p := provider.Detect(ps, u)
			if tc.want == "" {
				if p != nil {
					t.Errorf("expected no match; got %q", p.Name())
				}
				return
			}
			if p == nil {
				t.Fatalf("no provider matched %q", tc.raw)
			}
			if p.Name() != tc.want {
				t.Errorf("matched %q, want %q", p.Name(), tc.want)
			}
		})
	}
}

func TestByName(t *testing.T) {
	gh := &fakeProvider{name: "github"}
	gl := &fakeProvider{name: "gitlab"}
	ps := []provider.Provider{gh, gl}

	if p := provider.ByName(ps, "github"); p == nil || p.Name() != "github" {
		t.Errorf("ByName(github) = %v", p)
	}
	if p := provider.ByName(ps, "bitbucket"); p != nil {
		t.Errorf("ByName(bitbucket) expected nil; got %v", p.Name())
	}
}

func TestParseVisibility(t *testing.T) {
	tests := []struct {
		in      string
		want    provider.Visibility
		wantErr bool
	}{
		{"private", provider.VisibilityPrivate, false},
		{"PUBLIC", provider.VisibilityPublic, false},
		{"  internal  ", provider.VisibilityInternal, false},
		{"", provider.VisibilityUnspecified, false},
		{"unspecified", provider.VisibilityUnspecified, false},
		{"secret", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := provider.ParseVisibility(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %v; want %v", got, tc.want)
			}
		})
	}
}

func TestVisibility_String(t *testing.T) {
	cases := map[provider.Visibility]string{
		provider.VisibilityUnspecified: "unspecified",
		provider.VisibilityPrivate:     "private",
		provider.VisibilityPublic:      "public",
		provider.VisibilityInternal:    "internal",
	}
	for v, want := range cases {
		if got := v.String(); got != want {
			t.Errorf("%d.String() = %q; want %q", v, got, want)
		}
	}
}

// ---- test fake provider ----

type fakeProvider struct {
	name  string
	hosts []string
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Matches(u *url.URL) bool {
	for _, h := range f.hosts {
		if u.Host == h {
			return true
		}
	}
	return false
}

func (f *fakeProvider) ParseRepo(u *url.URL) (string, string, error) {
	return provider.SplitPath(u)
}

func (f *fakeProvider) RepoExists(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

func (f *fakeProvider) CreateRepo(_ context.Context, _ provider.CreateOptions) error {
	return nil
}

func (f *fakeProvider) AuthURL(u *url.URL) (string, error) {
	return u.String(), nil
}
