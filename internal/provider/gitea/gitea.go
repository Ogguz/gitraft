// Package gitea is the Gitea implementation of the provider.Provider interface.
// Gitea is dominantly self-hosted in practice — the provider matches NO URL
// unless a host is configured (via the CLI's --gitea-url flag), mirroring the
// Bitbucket Server pattern.
//
// API differences from siblings:
//   - REST API root is <host>/api/v1
//   - Auth via "Authorization: token <PAT>" header (Gitea-native scheme);
//     URL embedding uses the token as the URL username for HTTPS clone/push
//   - Owner type detection: GET /orgs/{owner} returns 200 with org id (verified
//     defensively) for orgs, 404 for users
//   - "private" is a single boolean — Gitea has no per-repo "internal" tier;
//     CLI surfaces a warning before this collapse happens
//   - Already-exists is 422 Unprocessable Entity with "already exists" message
package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Ogguz/gitraft/internal/provider"
)

const apiPath = "/api/v1"

// Options configures a Gitea provider.
type Options struct {
	// Host is the Gitea hostname (e.g., "gitea.example.com" or "codeberg.org").
	// Empty means "not configured" — Matches will return false for every URL.
	Host string
	// Token is a Gitea Personal Access Token. Empty means unauthenticated.
	Token string
	// BaseURL overrides the REST API root. Empty computes "https://<host>/api/v1".
	BaseURL string
	// HTTPClient overrides the HTTP client. Defaults to one with a 30s timeout
	// and redirect-following disabled.
	HTTPClient *http.Client
}

// New constructs a Gitea provider.
func New(opts Options) *Provider {
	base := opts.BaseURL
	if base == "" && opts.Host != "" {
		base = "https://" + opts.Host + apiPath
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: refuseRedirect,
		}
	}
	return &Provider{
		host:    strings.ToLower(opts.Host),
		token:   opts.Token,
		baseURL: strings.TrimRight(base, "/"),
		client:  client,
	}
}

func refuseRedirect(req *http.Request, via []*http.Request) error {
	prev := via[len(via)-1].URL
	return fmt.Errorf("gitea: refusing redirect (%s → %s); repo may have been moved\nhint: copy the current repository URL from your Gitea instance's web UI and rerun gitraft with the updated URL", prev, req.URL)
}

// Provider is the Gitea implementation of provider.Provider.
type Provider struct {
	host    string
	token   string
	baseURL string
	client  *http.Client
}

// Name implements [provider.Provider].
func (p *Provider) Name() string { return "gitea" }

// Matches returns true only when this provider has been configured with a
// host AND the URL matches that host. Gitea has no public-SaaS default —
// users must configure --gitea-url to engage the provider.
func (p *Provider) Matches(u *url.URL) bool {
	if p.host == "" {
		return false
	}
	return strings.ToLower(u.Hostname()) == p.host
}

// ParseRepo implements [provider.Provider] using the canonical /owner/repo split.
func (p *Provider) ParseRepo(u *url.URL) (string, string, error) {
	return provider.SplitPath(u)
}

// RepoExists implements [provider.Provider]. Errors when no host has been
// configured (Gitea has no public-SaaS default).
func (p *Provider) RepoExists(ctx context.Context, owner, name string) (bool, error) {
	if err := p.requireConfigured(); err != nil {
		return false, err
	}
	path := "/repos/" + owner + "/" + name
	req, err := p.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return false, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("gitea: GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, p.apiError(resp, "GET "+path)
	}
}

// CreateRepo implements [provider.Provider]. Routes to /orgs/<owner>/repos
// for organization owners, /user/repos for user owners (with a
// token-owner match check); treats 422/409 with an "already exists"
// envelope body as [provider.ErrRepoAlreadyExists] (benign race).
func (p *Provider) CreateRepo(ctx context.Context, opts provider.CreateOptions) error {
	if err := p.requireConfigured(); err != nil {
		return err
	}
	isOrg, err := p.isOrganization(ctx, opts.Owner)
	if err != nil {
		return err
	}
	var path string
	if isOrg {
		path = "/orgs/" + opts.Owner + "/repos"
	} else {
		// Personal repo creation goes through /user/repos which always creates
		// under the authenticated user. Verify the token's owner matches the
		// requested owner so we don't silently create under the wrong account.
		authedLogin, err := p.authenticatedLogin(ctx)
		if err != nil {
			return err
		}
		if !strings.EqualFold(authedLogin, opts.Owner) {
			return fmt.Errorf("gitea: cannot create repo under %q; the token is for %q (verify the owner spelling and that GITEA_TOKEN is owned by %q)", opts.Owner, authedLogin, opts.Owner)
		}
		path = "/user/repos"
	}

	body := map[string]any{
		"name": opts.Name,
		// "private" is true unless visibility is explicitly Public. Gitea has
		// no per-repo "internal" tier — VisibilityInternal collapses to private
		// here. The CLI surfaces a warning before this point so users aren't
		// silently surprised.
		"private": opts.Visibility != provider.VisibilityPublic,
		// Explicitly opt out of auto-init so Gitea doesn't create a stray
		// initial commit before the migration mirror push, which would
		// produce a divergent-history conflict the user wouldn't trace back
		// to gitraft.
		"auto_init": false,
	}
	if desc := strings.TrimSpace(opts.Description); desc != "" {
		body["description"] = desc
	}
	req, err := p.newRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("gitea: POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	// Gitea returns 422 (Unprocessable Entity) for duplicate names; some flows
	// produce 409.
	if resp.StatusCode == http.StatusUnprocessableEntity || resp.StatusCode == http.StatusConflict {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if isAlreadyExists(b) {
			return provider.ErrRepoAlreadyExists
		}
		return fmt.Errorf("gitea: POST %s: %s: %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return p.apiError(resp, "POST "+path)
	}
	return nil
}

// AuthURL implements [provider.Provider]; embeds the token as the URL
// username (no password — Gitea's documented HTTP-token clone-URL form).
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
		return "", fmt.Errorf("gitea: URL for %s already has embedded credentials\nhint: remove the userinfo from the URL, or unset GITEA_TOKEN to fall back to the URL's embedded credentials", u.Hostname())
	}
	authed := *u
	// Token as URL username (no password) — Gitea's documented HTTP-token
	// auth scheme for clone/push URLs.
	authed.User = url.User(p.token)
	return authed.String(), nil
}

// isOrganization checks whether owner is a Gitea organization. Returns false
// (without error) when owner is a user or doesn't exist; the subsequent
// CreateRepo call surfaces the not-found case more specifically. Errors at
// other status codes are wrapped with type-detection context so the user
// understands the org-detection step failed (vs the create step itself).
func (p *Provider) isOrganization(ctx context.Context, owner string) (bool, error) {
	req, err := p.newRequest(ctx, http.MethodGet, "/orgs/"+owner, nil)
	if err != nil {
		return false, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("gitea: GET /orgs/%s: %w", owner, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		// Validate the response actually describes an organization rather
		// than trusting the status code alone — guards against API drift
		// or proxies serving wrong responses.
		var body struct {
			ID int `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return false, fmt.Errorf("gitea: decode /orgs/%s: %w", owner, err)
		}
		if body.ID == 0 {
			return false, fmt.Errorf("gitea: /orgs/%s returned 200 without an organization id", owner)
		}
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("gitea: cannot determine if %q is an organization: %w", owner, p.apiError(resp, "GET /orgs/"+owner))
	}
}

// authenticatedLogin returns the login of the token's owner.
func (p *Provider) authenticatedLogin(ctx context.Context) (string, error) {
	req, err := p.newRequest(ctx, http.MethodGet, "/user", nil)
	if err != nil {
		return "", err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("gitea: GET /user: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", p.apiError(resp, "GET /user")
	}
	var body struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("gitea: decode /user: %w", err)
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
	req.Header.Set("Accept", "application/json")
	if p.token != "" {
		req.Header.Set("Authorization", "token "+p.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// requireConfigured returns an error if no host or BaseURL was configured.
// Without one, API calls would fail with a confusing low-level URL parse
// error far from the root cause (missing --gitea-url).
func (p *Provider) requireConfigured() error {
	if p.baseURL == "" {
		return errors.New("gitea: not configured\nhint: set --gitea-url=<your gitea URL> (or the `providers.gitea.url` field in the config file) before migrating to a Gitea instance")
	}
	return nil
}

func (p *Provider) apiError(resp *http.Response, op string) error {
	if resp.StatusCode == http.StatusTooManyRequests {
		if retry := resp.Header.Get("Retry-After"); retry != "" {
			return fmt.Errorf("gitea: %s: rate limited; retry after %s seconds\nhint: wait the requested seconds before retrying", op, retry)
		}
		return fmt.Errorf("gitea: %s: rate limited\nhint: wait before retrying; ask your Gitea admin if the rate-limit threshold is too aggressive for migration workflows", op)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		if p.token == "" {
			return fmt.Errorf("gitea: %s: 401 Unauthorized\nhint: GITEA_TOKEN unset; private repos require credentials", op)
		}
		return fmt.Errorf("gitea: %s: 401 Unauthorized\nhint: verify GITEA_TOKEN is valid and has not expired or been revoked, and that the user can access the repo via the web UI", op)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		return fmt.Errorf("gitea: %s: %s", op, resp.Status)
	}
	return fmt.Errorf("gitea: %s: %s: %s", op, resp.Status, msg)
}

// isAlreadyExists detects Gitea's duplicate-repo error in the standard
// `{"message": "..."}` envelope. Falls back to raw substring when JSON
// decode fails (proxy HTML pages, etc.).
func isAlreadyExists(body []byte) bool {
	var resp struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &resp); err == nil {
		if resp.Message != "" {
			return substringAlreadyExists(resp.Message)
		}
		return false
	}
	return substringAlreadyExists(string(body))
}

func substringAlreadyExists(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "already exists") || strings.Contains(lower, "has already been taken")
}
