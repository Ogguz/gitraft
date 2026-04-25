// Package github is the GitHub implementation of the provider.Provider interface.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Ogguz/gitraft/internal/provider"
)

const defaultBaseURL = "https://api.github.com"

// Options configures a GitHub provider.
type Options struct {
	// Token is a Personal Access Token or installation token. Empty means
	// unauthenticated (read-only, public-repo-only).
	Token string
	// BaseURL overrides the API endpoint. Used for GitHub Enterprise or tests.
	BaseURL string
	// HTTPClient overrides the HTTP client. Defaults to one with a 30s
	// timeout and redirect-following disabled.
	HTTPClient *http.Client
}

// New constructs a GitHub provider with the given options.
func New(opts Options) *Provider {
	base := opts.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: refuseRedirect,
		}
	}
	return &Provider{
		token:   opts.Token,
		baseURL: strings.TrimRight(base, "/"),
		client:  client,
	}
}

// refuseRedirect refuses HTTP redirects. A redirect from api.github.com
// usually means the repo was renamed — silently following could mutate a
// different repository than the user asked for.
func refuseRedirect(req *http.Request, via []*http.Request) error {
	prev := via[len(via)-1].URL
	return fmt.Errorf("github: refusing redirect (%s → %s); repo may have been renamed\nhint: copy the current repository URL from GitHub's web UI and rerun gitraft with the updated URL", prev, req.URL)
}

// Provider is the GitHub implementation of provider.Provider.
type Provider struct {
	token   string
	baseURL string
	client  *http.Client
}

func (p *Provider) Name() string { return "github" }

func (p *Provider) Matches(u *url.URL) bool {
	// Hostname strips any port (e.g. github.com:443).
	h := strings.ToLower(u.Hostname())
	return h == "github.com" || h == "www.github.com"
}

func (p *Provider) ParseRepo(u *url.URL) (string, string, error) {
	return provider.SplitPath(u)
}

func (p *Provider) RepoExists(ctx context.Context, owner, name string) (bool, error) {
	req, err := p.newRequest(ctx, http.MethodGet, "/repos/"+owner+"/"+name, nil)
	if err != nil {
		return false, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("github: GET /repos/%s/%s: %w", owner, name, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, p.apiError(resp, fmt.Sprintf("GET /repos/%s/%s", owner, name))
	}
}

func (p *Provider) CreateRepo(ctx context.Context, opts provider.CreateOptions) error {
	kind, err := p.ownerKind(ctx, opts.Owner)
	if err != nil {
		return err
	}
	var path string
	switch kind {
	case "Organization":
		path = "/orgs/" + opts.Owner + "/repos"
	case "User":
		authed, err := p.authenticatedLogin(ctx)
		if err != nil {
			return err
		}
		if !strings.EqualFold(authed, opts.Owner) {
			return fmt.Errorf("github: cannot create repo under %q; token is for %q", opts.Owner, authed)
		}
		path = "/user/repos"
	default:
		return fmt.Errorf("github: unknown owner kind %q for %q", kind, opts.Owner)
	}

	body := map[string]any{
		"name":        opts.Name,
		"description": opts.Description,
		// GitHub has no "internal" on personal repos — map it to private for a safe default.
		"private": opts.Visibility == provider.VisibilityPrivate || opts.Visibility == provider.VisibilityInternal,
	}
	req, err := p.newRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("github: POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	// 422 "name already exists on this account" and 409 Conflict both indicate
	// a benign race — surface as ErrRepoAlreadyExists so callers can continue.
	if resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusUnprocessableEntity {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if strings.Contains(strings.ToLower(string(b)), "already exists") {
			return provider.ErrRepoAlreadyExists
		}
		return fmt.Errorf("github: POST %s: %s: %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return p.apiError(resp, "POST "+path)
	}
	return nil
}

func (p *Provider) AuthURL(u *url.URL) (string, error) {
	if strings.EqualFold(u.Scheme, "ssh") {
		return u.String(), nil
	}
	if p.token == "" {
		return u.String(), nil
	}
	if !strings.EqualFold(u.Scheme, "https") && !strings.EqualFold(u.Scheme, "http") {
		return u.String(), nil
	}
	if u.User != nil {
		// Refuse to silently clobber credentials the user embedded.
		return "", fmt.Errorf("github: URL for %s already has embedded credentials\nhint: remove the userinfo from the URL, or unset GITHUB_TOKEN to fall back to the URL's embedded credentials", u.Hostname())
	}
	authed := *u
	authed.User = url.UserPassword("x-access-token", p.token)
	return authed.String(), nil
}

// ownerKind returns "User" or "Organization" for the given owner.
func (p *Provider) ownerKind(ctx context.Context, owner string) (string, error) {
	req, err := p.newRequest(ctx, http.MethodGet, "/users/"+owner, nil)
	if err != nil {
		return "", err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("github: GET /users/%s: %w", owner, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", p.apiError(resp, "GET /users/"+owner)
	}
	var body struct {
		Type string `json:"type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("github: decode /users/%s: %w", owner, err)
	}
	return body.Type, nil
}

// authenticatedLogin returns the login of the token's owner.
func (p *Provider) authenticatedLogin(ctx context.Context) (string, error) {
	req, err := p.newRequest(ctx, http.MethodGet, "/user", nil)
	if err != nil {
		return "", err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("github: GET /user: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", p.apiError(resp, "GET /user")
	}
	var body struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("github: decode /user: %w", err)
	}
	return body.Login, nil
}

func (p *Provider) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// apiError is a method (not a free function) so it can branch the 401
// hint on token state: empty-token vs bad/expired-token surface different
// remediations, mirroring the pattern in gitlab/gitea/bitbucket/
// bitbucketserver. Pre-method-conversion the github 401 path emitted a
// single hint that conflated the two — the empty-token user got told to
// "verify GITHUB_TOKEN is set" when the actionable advice for them is
// "you haven't set one; set it".
func (p *Provider) apiError(resp *http.Response, op string) error {
	if resp.StatusCode == http.StatusTooManyRequests {
		if retry := resp.Header.Get("Retry-After"); retry != "" {
			return fmt.Errorf("github: %s: rate limited; retry after %s seconds\nhint: wait the requested seconds before retrying; for higher quotas authenticate via GITHUB_TOKEN (5,000 req/hr authenticated vs 60 req/hr unauthenticated)", op, retry)
		}
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return fmt.Errorf("github: %s: rate limit quota exhausted\nhint: wait until the next rate-limit window or authenticate with a higher-quota token (set GITHUB_TOKEN to a Personal Access Token)", op)
		}
		return fmt.Errorf("github: %s: rate limited\nhint: wait before retrying; if the limit hits frequently, set GITHUB_TOKEN to lift quota from anonymous to authenticated", op)
	}
	// 401 surfaces a `\nhint:` preamble so consumers can grep `^hint:`
	// uniformly across providers. Empty / wrong / expired token surface
	// different hints — `repo` scope advice covers classic PATs; the
	// fine-grained PAT advice names the actual permission knob.
	if resp.StatusCode == http.StatusUnauthorized {
		if p.token == "" {
			return fmt.Errorf("github: %s: 401 Unauthorized\nhint: GITHUB_TOKEN unset; private repos require a Personal Access Token with `repo` scope (or a fine-grained PAT with `Contents: read/write` on the target repo)", op)
		}
		return fmt.Errorf("github: %s: 401 Unauthorized\nhint: verify GITHUB_TOKEN has not expired or been revoked, and that it has `repo` scope (or a fine-grained PAT with `Contents: read/write` on the target repo)", op)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		return fmt.Errorf("github: %s: %s", op, resp.Status)
	}
	return fmt.Errorf("github: %s: %s: %s", op, resp.Status, msg)
}
