// Package bitbucketserver is the Bitbucket Server / Data Center implementation
// of the provider.Provider interface. Bitbucket Server is purely self-hosted —
// the provider matches NO URL unless a host is configured (via the CLI's
// --bitbucket-url flag).
//
// API differences from Bitbucket Cloud (sibling package):
//   - REST API root is <host>/rest/api/1.0 (v1, not v2)
//   - URL shapes: /scm/{project}/{repo}.git (HTTPS clone),
//     /projects/{project}/repos/{repo} (browser), /{project}/{repo}.git (SSH)
//   - Auth via HTTP Basic with username + HTTP access token (PAT)
//   - Per-repo "public" boolean exists on create endpoint
//   - Error envelope is {"errors": [...]} (plural array)
package bitbucketserver

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

const apiPath = "/rest/api/1.0"

// Options configures a Bitbucket Server provider.
type Options struct {
	// Host is the Bitbucket Server hostname (e.g., "bitbucket.example.com").
	// Empty means "not configured" — Matches will return false for every URL.
	Host string
	// Username is the basic-auth username paired with Token. Both must be set
	// for auth to engage.
	Username string
	// Token is an HTTP access token (Personal Access Token).
	Token string
	// BaseURL overrides the REST API root. Empty computes "https://<host>/rest/api/1.0".
	BaseURL string
	// HTTPClient overrides the HTTP client. Defaults to one with a 30s timeout
	// and redirect-following disabled.
	HTTPClient *http.Client
}

// New constructs a Bitbucket Server provider.
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
		host:     strings.ToLower(opts.Host),
		username: opts.Username,
		token:    opts.Token,
		baseURL:  strings.TrimRight(base, "/"),
		client:   client,
	}
}

func refuseRedirect(req *http.Request, via []*http.Request) error {
	prev := via[len(via)-1].URL
	return fmt.Errorf("bitbucket-server: refusing redirect (%s → %s); repository may have been moved", prev, req.URL)
}

// Provider is the Bitbucket Server implementation of provider.Provider.
type Provider struct {
	host     string
	username string
	token    string
	baseURL  string
	client   *http.Client
}

func (p *Provider) Name() string { return "bitbucket-server" }

// Matches returns true only when this provider has been configured with a
// host AND the URL matches that host. Bitbucket Server has no default — an
// unconfigured provider deliberately matches nothing.
func (p *Provider) Matches(u *url.URL) bool {
	if p.host == "" {
		return false
	}
	return strings.ToLower(u.Hostname()) == p.host
}

// ParseRepo returns the project key and repository slug from any of the URL
// shapes Bitbucket Server uses:
//
//   - /scm/{project}/{repo}.git              (HTTPS clone URL)
//   - /projects/{project}/repos/{repo}       (browser URL)
//   - /{project}/{repo}.git                  (SSH URL after parsing)
//   - /{context}/scm/{project}/{repo}.git    (HTTPS clone behind context path)
//   - /scm/~{user}/{repo}.git                (personal repo, ~user is the project key)
//
// The function scans path segments rather than fixing positions so context
// paths work without configuration. The "scm" / "projects" tokens are matched
// case-sensitively — Bitbucket Server's project keys default to uppercase, so
// these lowercase tokens are unambiguous URL conventions, not project keys.
func (p *Provider) ParseRepo(u *url.URL) (string, string, error) {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := 0; i < len(parts); i++ {
		switch parts[i] {
		case "projects":
			if i+3 < len(parts) && parts[i+2] == "repos" {
				return parts[i+1], strings.TrimSuffix(parts[i+3], ".git"), nil
			}
		case "scm":
			if i+2 < len(parts) {
				return parts[i+1], strings.TrimSuffix(parts[i+2], ".git"), nil
			}
		}
	}
	// SSH form: /PROJECT/repo.git — exactly two segments and the first is not
	// a reserved Bitbucket Server prefix (which would mean a malformed URL
	// like /scm/PROJ missing the repo segment).
	if len(parts) == 2 && parts[0] != "scm" && parts[0] != "projects" {
		return parts[0], strings.TrimSuffix(parts[1], ".git"), nil
	}
	return "", "", fmt.Errorf("bitbucket-server: URL %q path is not /scm/{project}/{repo} or /projects/{project}/repos/{repo}", u)
}

func (p *Provider) RepoExists(ctx context.Context, project, repo string) (bool, error) {
	if err := p.requireConfigured(); err != nil {
		return false, err
	}
	path := "/projects/" + project + "/repos/" + repo
	req, err := p.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return false, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("bitbucket-server: GET %s: %w", path, err)
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

func (p *Provider) CreateRepo(ctx context.Context, opts provider.CreateOptions) error {
	if err := p.requireConfigured(); err != nil {
		return err
	}
	body := map[string]any{
		"name":  opts.Name,
		"scmId": "git",
		// forkable is hardcoded true to match the Bitbucket Server UI default.
		// No CLI knob — users who need non-forkable repos can flip it after
		// create through Bitbucket's repo settings.
		"forkable": true,
		// "public" is true only when explicitly Public; everything else
		// (Private, Internal, Unspecified) is non-public. Note Bitbucket
		// Server inherits real visibility from the parent project's settings;
		// this flag controls anonymous-read on the repo specifically.
		"public": opts.Visibility == provider.VisibilityPublic,
	}
	if desc := strings.TrimSpace(opts.Description); desc != "" {
		body["description"] = desc
	}
	path := "/projects/" + opts.Owner + "/repos"
	req, err := p.newRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("bitbucket-server: POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusBadRequest {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if isAlreadyExists(b) {
			return provider.ErrRepoAlreadyExists
		}
		return fmt.Errorf("bitbucket-server: POST %s: %s: %s", path, resp.Status, strings.TrimSpace(string(b)))
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
	switch {
	case p.username == "" && p.token == "":
		return u.String(), nil
	case p.username == "" || p.token == "":
		return "", fmt.Errorf("bitbucket-server: BITBUCKET_SERVER_USERNAME and BITBUCKET_SERVER_TOKEN must be set together (one is empty)")
	}
	if !strings.EqualFold(u.Scheme, "https") && !strings.EqualFold(u.Scheme, "http") {
		return u.String(), nil
	}
	if u.User != nil {
		return "", fmt.Errorf("bitbucket-server: URL for %s already has embedded credentials; remove them or unset BITBUCKET_SERVER_USERNAME/BITBUCKET_SERVER_TOKEN", u.Hostname())
	}
	authed := *u
	authed.User = url.UserPassword(p.username, p.token)
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
	if p.username != "" && p.token != "" {
		req.SetBasicAuth(p.username, p.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (p *Provider) apiError(resp *http.Response, op string) error {
	if resp.StatusCode == http.StatusTooManyRequests {
		if retry := resp.Header.Get("Retry-After"); retry != "" {
			return fmt.Errorf("bitbucket-server: %s: rate limited; retry after %s seconds", op, retry)
		}
		return fmt.Errorf("bitbucket-server: %s: rate limited", op)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		if p.username == "" || p.token == "" {
			return fmt.Errorf("bitbucket-server: %s: 401 Unauthorized (BITBUCKET_SERVER_USERNAME/BITBUCKET_SERVER_TOKEN unset; private repos require credentials)", op)
		}
		return fmt.Errorf("bitbucket-server: %s: 401 Unauthorized (verify BITBUCKET_SERVER_TOKEN has not expired or been revoked)", op)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		return fmt.Errorf("bitbucket-server: %s: %s", op, resp.Status)
	}
	return fmt.Errorf("bitbucket-server: %s: %s: %s", op, resp.Status, msg)
}

// isAlreadyExists detects Bitbucket Server's duplicate-repo error in the
// `{"errors": [{"message": "...", "exceptionName": "..."}]}` envelope.
// Falls back to raw substring only when the body isn't valid JSON.
func isAlreadyExists(body []byte) bool {
	var resp struct {
		Errors []struct {
			Message       string `json:"message"`
			ExceptionName string `json:"exceptionName"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err == nil {
		for _, e := range resp.Errors {
			if substringAlreadyExists(e.Message) {
				return true
			}
			// Match specific repo-creation exception classes by suffix; a
			// generic substring on "taken" would over-match unrelated
			// exceptions such as ResourceLockTakenException.
			if strings.HasSuffix(e.ExceptionName, "RepositorySlugTakenException") ||
				strings.HasSuffix(e.ExceptionName, "RepositoryNameTakenException") {
				return true
			}
		}
		return false
	}
	return substringAlreadyExists(string(body))
}

// requireConfigured returns an error if no host or BaseURL was configured.
// Without one, API calls would fail with a confusing low-level URL parse
// error far from the root cause.
func (p *Provider) requireConfigured() error {
	if p.baseURL == "" {
		return errors.New("bitbucket-server: not configured; set --bitbucket-url=<your bitbucket server URL>")
	}
	return nil
}

func substringAlreadyExists(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "already exists") ||
		strings.Contains(lower, "already taken") ||
		strings.Contains(lower, "is already taken")
}
