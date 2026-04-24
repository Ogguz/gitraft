// Package provider defines the hosting-provider abstraction used by gitraft
// to auto-detect providers from URLs, create destination repositories, and
// embed auth tokens into push URLs.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Visibility of a repository. VisibilityUnspecified means "use the provider's default".
type Visibility int

const (
	VisibilityUnspecified Visibility = iota
	VisibilityPrivate
	VisibilityPublic
	VisibilityInternal
)

// String renders Visibility in canonical lowercase form.
func (v Visibility) String() string {
	switch v {
	case VisibilityPrivate:
		return "private"
	case VisibilityPublic:
		return "public"
	case VisibilityInternal:
		return "internal"
	default:
		return "unspecified"
	}
}

// ParseVisibility parses a string form (case-insensitive). Accepts
// "private", "public", "internal", or empty/"unspecified".
func ParseVisibility(s string) (Visibility, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "private":
		return VisibilityPrivate, nil
	case "public":
		return VisibilityPublic, nil
	case "internal":
		return VisibilityInternal, nil
	case "", "unspecified":
		return VisibilityUnspecified, nil
	default:
		return VisibilityUnspecified, fmt.Errorf("unknown visibility %q (expected private|public|internal)", s)
	}
}

// ErrRepoAlreadyExists signals that a CreateRepo call raced with another
// creation and the destination already exists. Callers should treat this
// as "repo is ready to push to", not a hard failure.
var ErrRepoAlreadyExists = errors.New("repository already exists")

// Provider represents a git hosting service (GitHub, GitLab, Bitbucket, Gitea).
type Provider interface {
	Name() string
	Matches(u *url.URL) bool
	ParseRepo(u *url.URL) (owner, name string, err error)
	RepoExists(ctx context.Context, owner, name string) (bool, error)
	CreateRepo(ctx context.Context, opts CreateOptions) error
	AuthURL(u *url.URL) (string, error)
}

// CreateOptions describes a repository being created at a provider.
type CreateOptions struct {
	Owner       string
	Name        string
	Description string
	Visibility  Visibility
}

// Parse parses a git repository URL, accepting both standard URL forms
// (https://host/owner/repo.git, ssh://user@host/path) and the scp-like
// SSH form (git@host:owner/repo.git) that git itself accepts.
func Parse(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty URL")
	}
	// Convert scp-like SSH (user@host:path) to ssh://user@host/path so the
	// stdlib url package can parse it.
	if !strings.Contains(raw, "://") {
		at := strings.Index(raw, "@")
		colon := strings.Index(raw, ":")
		if at != -1 && colon > at {
			raw = "ssh://" + raw[:colon] + "/" + raw[colon+1:]
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", raw, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("URL %q has no host", raw)
	}
	return u, nil
}

// Detect returns the first provider in ps that handles u, or nil if none match.
func Detect(ps []Provider, u *url.URL) Provider {
	for _, p := range ps {
		if p.Matches(u) {
			return p
		}
	}
	return nil
}

// ByName returns the provider in ps whose Name() matches, or nil.
func ByName(ps []Provider, name string) Provider {
	for _, p := range ps {
		if p.Name() == name {
			return p
		}
	}
	return nil
}

// SplitPath splits a URL path of the form "/owner/repo[.git]" into owner, name.
// Returns an error when the path does not have exactly two segments —
// providers that support nested paths (e.g., GitLab subgroups) implement
// their own ParseRepo rather than using this helper.
func SplitPath(u *url.URL) (owner, name string, err error) {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("URL %q path must be /owner/repo; got %d segments", u, len(parts))
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), nil
}
