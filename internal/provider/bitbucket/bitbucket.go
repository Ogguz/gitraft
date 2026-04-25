// Package bitbucket is the Bitbucket Cloud (bitbucket.org) implementation of
// the provider.Provider interface. Bitbucket Server / Data Center has a
// different API and lives in a sibling package.
package bitbucket

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

const defaultBaseURL = "https://api.bitbucket.org/2.0"

// Options configures a Bitbucket Cloud provider. Bitbucket Cloud auth uses
// HTTP Basic with a username and an app password (a scoped credential, NOT
// the user's account password).
type Options struct {
	Username    string
	AppPassword string
	BaseURL     string
	HTTPClient  *http.Client
}

// New constructs a Bitbucket Cloud provider.
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
		username:    opts.Username,
		appPassword: opts.AppPassword,
		baseURL:     strings.TrimRight(base, "/"),
		client:      client,
	}
}

func refuseRedirect(req *http.Request, via []*http.Request) error {
	prev := via[len(via)-1].URL
	return fmt.Errorf("bitbucket: refusing redirect (%s → %s); repository may have been moved\nhint: copy the current repository URL from Bitbucket's web UI and rerun gitraft with the updated URL", prev, req.URL)
}

// Provider is the Bitbucket Cloud implementation of provider.Provider.
type Provider struct {
	username    string
	appPassword string
	baseURL     string
	client      *http.Client
}

// Name implements [provider.Provider].
func (p *Provider) Name() string { return "bitbucket" }

// Matches implements [provider.Provider]; matches bitbucket.org and www.bitbucket.org.
func (p *Provider) Matches(u *url.URL) bool {
	h := strings.ToLower(u.Hostname())
	return h == "bitbucket.org" || h == "www.bitbucket.org"
}

// ParseRepo extracts the workspace and repo slug from the URL path.
// Bitbucket Cloud always uses a flat workspace/repo_slug structure.
func (p *Provider) ParseRepo(u *url.URL) (string, string, error) {
	return provider.SplitPath(u)
}

// RepoExists implements [provider.Provider]; returns false on 404.
func (p *Provider) RepoExists(ctx context.Context, workspace, slug string) (bool, error) {
	req, err := p.newRequest(ctx, http.MethodGet, "/repositories/"+workspace+"/"+slug, nil)
	if err != nil {
		return false, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("bitbucket: GET /repositories/%s/%s: %w", workspace, slug, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, p.apiError(resp, fmt.Sprintf("GET /repositories/%s/%s", workspace, slug))
	}
}

// CreateRepo implements [provider.Provider]. Treats 400/409 with an
// "already exists" envelope body as [provider.ErrRepoAlreadyExists]
// (benign race), falls through to [Provider.apiError] otherwise.
func (p *Provider) CreateRepo(ctx context.Context, opts provider.CreateOptions) error {
	body := map[string]any{
		"scm": "git",
		// is_private is true unless visibility is explicitly Public. Bitbucket
		// Cloud has no native "internal" tier, so VisibilityInternal collapses
		// to private here (and VisibilityUnspecified does too — safe default
		// for a migration tool).
		"is_private":  opts.Visibility != provider.VisibilityPublic,
		"description": opts.Description,
	}
	path := "/repositories/" + opts.Owner + "/" + opts.Name
	req, err := p.newRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("bitbucket: POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	// Bitbucket usually returns 400 with "already exists" on duplicate slugs.
	// Some flows also produce 409.
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusConflict {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if isAlreadyExists(b) {
			return provider.ErrRepoAlreadyExists
		}
		return fmt.Errorf("bitbucket: POST %s: %s: %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return p.apiError(resp, "POST "+path)
	}
	return nil
}

// AuthURL implements [provider.Provider]; embeds username + app password
// as basic auth on https URLs.
func (p *Provider) AuthURL(u *url.URL) (string, error) {
	if strings.EqualFold(u.Scheme, "ssh") {
		return u.String(), nil
	}
	switch {
	case p.username == "" && p.appPassword == "":
		// Both unset — unauthenticated mode (the startup warnMissingTokens
		// warning already fired). Public repos work; private will 401.
		return u.String(), nil
	case p.username == "" || p.appPassword == "":
		// Half-set is configuration error, not a fallback — surface it loudly.
		return "", fmt.Errorf("bitbucket: BITBUCKET_USERNAME and BITBUCKET_APP_PASSWORD must be set together\nhint: only one of the two is set — set both, or unset both to fall back to anonymous access (public repos only)")
	}
	if !strings.EqualFold(u.Scheme, "https") && !strings.EqualFold(u.Scheme, "http") {
		return u.String(), nil
	}
	if u.User != nil {
		return "", fmt.Errorf("bitbucket: URL for %s already has embedded credentials\nhint: remove the userinfo from the URL, or unset BITBUCKET_USERNAME and BITBUCKET_APP_PASSWORD to fall back to the URL's embedded credentials", u.Hostname())
	}
	authed := *u
	authed.User = url.UserPassword(p.username, p.appPassword)
	return authed.String(), nil
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
	if p.username != "" && p.appPassword != "" {
		req.SetBasicAuth(p.username, p.appPassword)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// apiError turns a non-2xx response into an actionable error. Surfaces
// rate-limit headers and special-cases 401 when no credentials are set.
//
// App Password deprecation note: as of 2025-09-09 Atlassian no longer
// allows creating new App Passwords for Bitbucket Cloud, and existing
// App Passwords stop working entirely on 2026-06-09. The hints
// reference both `BITBUCKET_APP_PASSWORD` (current env var) and the
// API-token successor; the env-var rename is tracked separately to
// preserve config-file backward compatibility for users still on
// pre-deprecation App Passwords. After 2026-06-09 the App Password
// branch is dead — the env var should be aliased to BITBUCKET_API_TOKEN
// or similar in a follow-up.
func (p *Provider) apiError(resp *http.Response, op string) error {
	if resp.StatusCode == http.StatusTooManyRequests {
		if retry := resp.Header.Get("Retry-After"); retry != "" {
			return fmt.Errorf("bitbucket: %s: rate limited; retry after %s seconds\nhint: wait the requested seconds before retrying", op, retry)
		}
		return fmt.Errorf("bitbucket: %s: rate limited\nhint: wait before retrying; if rate limits hit frequently, authenticate with BITBUCKET_USERNAME plus BITBUCKET_APP_PASSWORD (or an Atlassian API token — App Passwords are deprecated and stop working 2026-06-09)", op)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		if p.username == "" || p.appPassword == "" {
			return fmt.Errorf("bitbucket: %s: 401 Unauthorized\nhint: BITBUCKET_USERNAME/BITBUCKET_APP_PASSWORD unset; private repos require credentials — Atlassian API tokens are the supported method (App Passwords are deprecated and stop working 2026-06-09; new ones can no longer be created since 2025-09-09)", op)
		}
		return fmt.Errorf("bitbucket: %s: 401 Unauthorized\nhint: verify the credential is valid and has not been revoked; BITBUCKET_USERNAME must be your Bitbucket username (the @-handle visible at https://bitbucket.org/account/settings/), NOT your Atlassian email — note: App Passwords are deprecated and stop working 2026-06-09, migrate to Atlassian API tokens", op)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		return fmt.Errorf("bitbucket: %s: %s", op, resp.Status)
	}
	return fmt.Errorf("bitbucket: %s: %s: %s", op, resp.Status, msg)
}

// isAlreadyExists detects Bitbucket's duplicate-slug error in both the
// flat string form and the structured `{error: {message, fields}}` envelope.
func isAlreadyExists(body []byte) bool {
	var resp struct {
		Error struct {
			Message string              `json:"message"`
			Fields  map[string][]string `json:"fields"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err == nil {
		// Trust the structured envelope when JSON decode succeeded — searching
		// raw bytes for "already exists" can false-positive on incidental text
		// elsewhere in the response (help URLs, hints, unrelated fields).
		if substringAlreadyExists(resp.Error.Message) {
			return true
		}
		for _, vs := range resp.Error.Fields {
			for _, v := range vs {
				if substringAlreadyExists(v) {
					return true
				}
			}
		}
		return false
	}
	// Body wasn't valid JSON — fall back to raw substring as a last resort.
	return substringAlreadyExists(string(body))
}

func substringAlreadyExists(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "already exists") || strings.Contains(lower, "has already been taken")
}
