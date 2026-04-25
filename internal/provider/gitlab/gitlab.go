// Package gitlab is the GitLab implementation of the provider.Provider interface.
// Supports gitlab.com by default and self-hosted instances via the Host option.
package gitlab

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

const (
	defaultHost = "gitlab.com"
	apiPath     = "/api/v4"
)

// Options configures a GitLab provider.
type Options struct {
	// Token is a Personal Access Token or Project Access Token. Empty means
	// unauthenticated (public projects only).
	Token string
	// Host is the GitLab hostname (e.g., "gitlab.com" or "gitlab.example.com").
	// Empty defaults to gitlab.com.
	Host string
	// BaseURL overrides the REST API root. Empty computes "https://<host>/api/v4".
	// Used for tests and unusual self-hosted setups.
	BaseURL string
	// HTTPClient overrides the HTTP client. Defaults to one with a 30s timeout
	// and redirect-following disabled.
	HTTPClient *http.Client
}

// New constructs a GitLab provider with the given options.
func New(opts Options) *Provider {
	host := opts.Host
	if host == "" {
		host = defaultHost
	}
	base := opts.BaseURL
	if base == "" {
		base = "https://" + host + apiPath
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
		host:    strings.ToLower(host),
		baseURL: strings.TrimRight(base, "/"),
		client:  client,
	}
}

// refuseRedirect returns an error that net/http surfaces as the request error.
// When this fires Go's Client.Do returns a non-nil response whose body is
// already closed; callers must check the error before dereferencing resp.
func refuseRedirect(req *http.Request, via []*http.Request) error {
	prev := via[len(via)-1].URL
	return fmt.Errorf("gitlab: refusing redirect (%s → %s); project may have been moved\nhint: copy the current project URL from GitLab's web UI and rerun gitraft with the updated URL", prev, req.URL)
}

// Provider is the GitLab implementation of provider.Provider.
type Provider struct {
	token   string
	host    string
	baseURL string
	client  *http.Client
}

func (p *Provider) Name() string { return "gitlab" }

func (p *Provider) Matches(u *url.URL) bool {
	return strings.ToLower(u.Hostname()) == p.host
}

// ParseRepo returns the namespace path as "owner" (supporting nested subgroups)
// and the project name as "name".
//
// Examples:
//
//	https://gitlab.com/mygroup/myproj.git        → owner="mygroup",           name="myproj"
//	https://gitlab.com/mygroup/sub/myproj.git    → owner="mygroup/sub",       name="myproj"
//	git@gitlab.com:parent/child/grand/proj.git   → owner="parent/child/grand", name="proj"
func (p *Provider) ParseRepo(u *url.URL) (owner, name string, err error) {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("gitlab: URL %q does not contain a namespace/project path\nhint: GitLab URLs need at least namespace/project (e.g. https://gitlab.com/group/proj.git or git@gitlab.com:group/sub/proj.git)", u)
	}
	name = strings.TrimSuffix(parts[len(parts)-1], ".git")
	if name == "" {
		return "", "", fmt.Errorf("gitlab: URL %q has empty project name after stripping .git\nhint: the last path segment must be the project name; check for a trailing slash or a stray .git", u)
	}
	owner = strings.Join(parts[:len(parts)-1], "/")
	return owner, name, nil
}

func (p *Provider) RepoExists(ctx context.Context, owner, name string) (bool, error) {
	projectID := url.PathEscape(owner + "/" + name)
	req, err := p.newRequest(ctx, http.MethodGet, "/projects/"+projectID, nil)
	if err != nil {
		return false, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("gitlab: GET /projects/%s: %w", projectID, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, p.apiError(resp, "GET /projects/"+projectID)
	}
}

func (p *Provider) CreateRepo(ctx context.Context, opts provider.CreateOptions) error {
	nsID, err := p.namespaceID(ctx, opts.Owner)
	if err != nil {
		return err
	}
	body := map[string]any{
		"name":         opts.Name,
		"path":         opts.Name,
		"namespace_id": nsID,
		"description":  opts.Description,
		"visibility":   visibilityString(opts.Visibility),
	}
	req, err := p.newRequest(ctx, http.MethodPost, "/projects", body)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("gitlab: POST /projects: %w", err)
	}
	defer resp.Body.Close()

	// GitLab returns 409 Conflict or 400 Bad Request with "has already been
	// taken" (in either a string or structured form) when the project already
	// exists — treat as a benign race.
	if resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusBadRequest {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if isAlreadyExists(b) {
			return provider.ErrRepoAlreadyExists
		}
		return fmt.Errorf("gitlab: POST /projects: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return p.apiError(resp, "POST /projects")
	}
	return nil
}

// visibilityString maps the provider enum to GitLab's visibility string.
// VisibilityUnspecified falls back to "private" — the safe default for a
// migration tool.
func visibilityString(v provider.Visibility) string {
	switch v {
	case provider.VisibilityPrivate:
		return "private"
	case provider.VisibilityPublic:
		return "public"
	case provider.VisibilityInternal:
		return "internal"
	default:
		return "private"
	}
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
		return "", fmt.Errorf("gitlab: URL for %s already has embedded credentials\nhint: remove the userinfo from the URL, or unset GITLAB_TOKEN to fall back to the URL's embedded credentials", u.Hostname())
	}
	authed := *u
	authed.User = url.UserPassword("oauth2", p.token)
	return authed.String(), nil
}

// namespaceID looks up the numeric ID of a namespace path (user or group,
// nested groups supported). Required by POST /projects.
//
// Distinct errors surface the two-phase nature of CreateRepo so users don't
// misdiagnose a missing group as a create-permission problem.
func (p *Provider) namespaceID(ctx context.Context, path string) (int, error) {
	encoded := url.PathEscape(path)
	req, err := p.newRequest(ctx, http.MethodGet, "/namespaces/"+encoded, nil)
	if err != nil {
		return 0, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("gitlab: resolving namespace %q: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// Scope note: this lookup is part of CreateRepo (the only caller),
		// which then performs POST /projects — that requires `api` (write)
		// scope, not `read_api`. Recommending only `read_api` here would
		// pass this call but fail the next, so the hint surfaces the
		// scope actually needed to complete the workflow.
		return 0, fmt.Errorf("gitlab: namespace %q not found or not visible to your token\nhint: verify the namespace exists (and has not been renamed/deleted) and that GITLAB_TOKEN has the `api` scope and is a member of the group (or owns the user namespace)", path)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, p.apiError(resp, "GET /namespaces/"+encoded)
	}
	var body struct {
		ID int `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("gitlab: decode /namespaces/%s: %w", encoded, err)
	}
	if body.ID == 0 {
		return 0, fmt.Errorf("gitlab: /namespaces/%s returned no id; expected a namespace object", encoded)
	}
	return body.ID, nil
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
		req.Header.Set("PRIVATE-TOKEN", p.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// apiError is a method so it can consult the provider's token state and
// produce actionable 401 messages when auth is unset.
func (p *Provider) apiError(resp *http.Response, op string) error {
	if resp.StatusCode == http.StatusTooManyRequests {
		if retry := resp.Header.Get("Retry-After"); retry != "" {
			return fmt.Errorf("gitlab: %s: rate limited; retry after %s seconds\nhint: wait the requested seconds before retrying", op, retry)
		}
		return fmt.Errorf("gitlab: %s: rate limited\nhint: wait before retrying; if the limit hits frequently, authenticate with GITLAB_TOKEN", op)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// Empty-token and bad-token cases both surface 401; the hint
		// covers both by pointing at the env var and its lifecycle.
		// Scope note: `apiError` is called from RepoExists (read) and
		// CreateRepo+namespaceID (write). The "for write operations"
		// clause acknowledges that read-only callers could technically
		// use `read_api`, but in gitraft today every reachable caller
		// is part of a write workflow (ensureDestination), so `api` is
		// the actual minimum. If a future read-only caller appears,
		// this hint should be threaded with operation context.
		if p.token == "" {
			return fmt.Errorf("gitlab: %s: 401 Unauthorized\nhint: GITLAB_TOKEN is unset; private projects require a token with `api` scope", op)
		}
		return fmt.Errorf("gitlab: %s: 401 Unauthorized\nhint: verify GITLAB_TOKEN has not expired or been revoked, and that it has at least `api` scope for write operations", op)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		return fmt.Errorf("gitlab: %s: %s", op, resp.Status)
	}
	return fmt.Errorf("gitlab: %s: %s: %s", op, resp.Status, msg)
}

// isAlreadyExists detects GitLab's "has already been taken" / "already exists"
// error body in both the string (`{"message":"..."}`) and structured
// (`{"message":{"name":["..."], "path":["..."]}}`) forms.
func isAlreadyExists(body []byte) bool {
	var resp struct {
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		// Non-JSON body; fall back to raw substring match.
		return substringMatchesAlreadyExists(string(body))
	}
	if len(resp.Message) == 0 {
		return false
	}
	var msgStr string
	if err := json.Unmarshal(resp.Message, &msgStr); err == nil {
		return substringMatchesAlreadyExists(msgStr)
	}
	var msgObj map[string][]string
	if err := json.Unmarshal(resp.Message, &msgObj); err == nil {
		for _, values := range msgObj {
			for _, v := range values {
				if substringMatchesAlreadyExists(v) {
					return true
				}
			}
		}
	}
	return false
}

func substringMatchesAlreadyExists(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "has already been taken") || strings.Contains(lower, "already exists")
}
