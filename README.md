# gitraft

> Migrate a git repository between hosts — with full history, LFS objects, and
> a destination repo created for you.

`gitraft` is a single Go binary that mirrors a git repository from any of the
five major hosting providers to any other, preserving every commit, branch,
tag, and LFS object along the way.

```
$ gitraft migrate https://github.com/acme/widgets.git https://gitlab.com/acme/widgets.git
INFO cloning source src=https://github.com/acme/widgets.git
INFO Git LFS detected; fetching object content
INFO creating destination provider=gitlab owner=acme name=widgets visibility=private
INFO pushing to destination dst=https://gitlab.com/acme/widgets.git
INFO pushing LFS objects dst=https://gitlab.com/acme/widgets.git
INFO temporary clone retained dir=/tmp/gitraft-NNN
```

No credential helpers to wire up. No glue scripts. No "how do I get LFS to
push" stack overflow expedition. Run one command; get the same repo at the
other end.

## Why gitraft

| Need | gitraft | bare `git push --mirror` |
|---|---|---|
| All branches + tags | ✓ | ✓ |
| LFS objects | ✓ (auto-detected) | manual `git lfs push --all` |
| Auto-create destination repo | ✓ | manual via web UI |
| Provider-aware auth (token in URL) | ✓ | manual `.git-credentials` |
| Submodule warnings | ✓ | silent omission |
| Cross-provider works | ✓ | yes, but gotchas |
| Interactive wizard | ✓ | ✗ |
| `--json` output for scripts | ✓ | ✗ |

## Installation

### Homebrew (macOS / Linux)

```bash
brew install Ogguz/gitraft/gitraft
```

### `go install`

```bash
go install github.com/Ogguz/gitraft/cmd/gitraft@latest
```

### Pre-built binaries

Download the archive for your OS/arch from
[releases](https://github.com/Ogguz/gitraft/releases). Each release ships a
SHA256 checksum file, an SBOM, and a cosign signature.

```bash
# Verify a release. Replace VERSION (e.g. 1.0.0) and PLATFORM (e.g.
# linux_amd64, darwin_arm64, windows_amd64) with the values matching
# the archive you downloaded. The certificate-identity must include the
# `v` prefix (vVERSION) — that's the literal git tag, distinct from the
# VERSION used in archive names.
VERSION=1.0.0
PLATFORM=linux_amd64

cosign verify-blob \
  --certificate-identity "https://github.com/Ogguz/gitraft/.github/workflows/release.yml@refs/tags/v${VERSION}" \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  --signature "gitraft_${VERSION}_${PLATFORM}.tar.gz.sig" \
  "gitraft_${VERSION}_${PLATFORM}.tar.gz"
```

### From source

```bash
git clone https://github.com/Ogguz/gitraft.git
cd gitraft
go build -o gitraft ./cmd/gitraft
```

## Quick start

### Interactive wizard (no arguments)

Launch from a terminal with no arguments to walk through source + destination
prompts:

```bash
gitraft
```

The wizard validates each URL against the same parser the CLI uses, so a paste
that won't work in one-shot mode fails fast in the wizard too.

### One-shot migration

```bash
gitraft migrate <SOURCE_URL> <DESTINATION_URL>
```

Both URLs accept any of:
- `https://host/owner/repo.git`
- `git@host:owner/repo.git` (scp-like SSH)
- `ssh://git@host/owner/repo.git`

`gitraft` auto-detects the provider from each URL's host and applies the
matching auth (env var) before any network operation.

## Real examples

### GitHub → GitLab (private → private)

```bash
export GITHUB_TOKEN=ghp_...   # source: read access
export GITLAB_TOKEN=glpat-... # destination: api scope (write)

gitraft migrate \
  https://github.com/acme/api.git \
  https://gitlab.com/acme/api.git
```

The destination is auto-created at `gitlab.com/acme/api` as **private** (the
default — safer than accidentally publishing a private repo). Override with
`--visibility=public` or `--visibility=internal`.

### Bitbucket Cloud → self-hosted Gitea

```bash
export BITBUCKET_USERNAME=alice
export BITBUCKET_APP_PASSWORD=ATBB...   # NOT your account password
export GITEA_TOKEN=...

gitraft migrate \
  --gitea-url=https://gitea.example.com \
  https://bitbucket.org/acme/payments.git \
  https://gitea.example.com/acme/payments.git
```

### LFS-heavy repository

LFS is auto-detected — there's no flag to enable it. If `git-lfs` isn't
installed, `gitraft` exits with a platform-specific install hint before
making any network call:

```
gitraft: source repository uses Git LFS but git-lfs is not installed.

Install git-lfs:
  brew install git-lfs
  sudo apt install git-lfs    (Debian/Ubuntu)
  see https://git-lfs.com for other distros

Then run `git lfs install` once to enable it for your user.
```

### Dry run

```bash
gitraft migrate --dry-run \
  https://github.com/acme/api.git \
  https://gitlab.com/acme/api.git
```

Prints every shell command the run would execute (clone, lfs fetch, push,
lfs push) without invoking any of them. Handy for sanity-checking a
migration plan in CI.

### Self-hosted GitLab → Bitbucket Server

```bash
export GITLAB_TOKEN=...
export BITBUCKET_SERVER_USERNAME=alice
export BITBUCKET_SERVER_TOKEN=...

gitraft migrate \
  --gitlab-url=https://gitlab.internal.example.com \
  --bitbucket-url=https://bitbucket.internal.example.com \
  https://gitlab.internal.example.com/team/proj.git \
  https://bitbucket.internal.example.com/scm/TEAM/proj.git
```

The `--bitbucket-url` flag is required to engage the Bitbucket Server
provider — otherwise `gitraft` would route the URL through Bitbucket Cloud's
provider and fail the auth check.

### GitHub Enterprise Server → self-hosted GitLab

```bash
export GITHUB_TOKEN=ghp_...   # GHE token (read access)
export GITLAB_TOKEN=glpat-... # destination GitLab token (api scope)

gitraft migrate \
  --github-url=https://github.example.com \
  --gitlab-url=https://gitlab.example.com \
  https://github.example.com/team/api.git \
  https://gitlab.example.com/team/api.git
```

The `--github-url` flag is required to route GHE URLs to the GitHub
provider (without it, `gitraft` falls back to anonymous git push since
no provider matches the GHE hostname). Pure cross-instance migrations
within GitHub itself (GHE ↔ github.com) need two runs: the
`--github-url` flag binds the github provider to a single host per run.

## Provider support matrix

| Provider | Source | Destination | Auto-create | LFS | Notes |
|---|---|---|---|---|---|
| GitHub.com | ✓ | ✓ | ✓ (user + org) | ✓ | `GITHUB_TOKEN`, `repo` scope |
| GitHub Enterprise Server | ✓ | ✓ | ✓ (user + org) | ✓ | `--github-url`, `GITHUB_TOKEN` |
| GitLab.com | ✓ | ✓ | ✓ (user + group + subgroup) | ✓ | `GITLAB_TOKEN`, `api` scope |
| GitLab self-hosted | ✓ | ✓ | ✓ | ✓ | `--gitlab-url` |
| Bitbucket Cloud | ✓ | ✓ | ✓ | ✓ | `BITBUCKET_APP_PASSWORD` (not account password) |
| Bitbucket Server / Data Center | ✓ | ✓ | ✓ | ✓ | `--bitbucket-url`, HTTP token |
| Gitea (self-hosted) | ✓ | ✓ | ✓ (user + org) | ✓ | `--gitea-url`, `GITEA_TOKEN` |

AWS CodeCommit and Azure DevOps are not yet supported and are tracked
under the [v2 milestone](https://github.com/Ogguz/gitraft/milestones).

## Configuration

`gitraft` reads settings from three sources, in priority order:

1. **CLI flags** — highest priority, always win.
2. **Environment variables** — `GITHUB_TOKEN`, `GITLAB_TOKEN`,
   `BITBUCKET_USERNAME`, `BITBUCKET_APP_PASSWORD`,
   `BITBUCKET_SERVER_USERNAME`, `BITBUCKET_SERVER_TOKEN`, `GITEA_TOKEN`.
3. **YAML config file** — at `$XDG_CONFIG_HOME/gitraft/config.yaml`
   (or `$HOME/.config/gitraft/config.yaml`).

Example config (all five providers; remove sections you don't use):

```yaml
providers:
  github:
    url: https://github.example.com   # omit for github.com (SaaS)
    token: ghp_xxx
  gitlab:
    url: https://gitlab.example.com   # omit for gitlab.com
    token: glpat-xxx
  bitbucket:                          # Bitbucket Cloud
    username: alice
    app_password: ATBB_xxx            # NOT your account password
  bitbucket-server:                   # self-hosted Bitbucket Server / Data Center
    url: https://bitbucket.example.com
    username: alice
    token: xxx
  gitea:
    url: https://gitea.example.com
    token: xxx
```

Note the key spelling: `bitbucket-server` is kebab-case (matches the
internal config struct tag); `app_password` is snake_case. Strict YAML
parsing rejects unknown keys, so a typo there fails fast rather than
silently dropping the value.

`gitraft` warns when the config file has world-readable permissions —
tokens belong on disk only when the file is `0600` or stricter.

## Output modes

### Default (human-readable)

Logs go to **stderr** in slog text format, with a phase-marker spinner on
TTYs:

```
INFO cloning source src=https://github.com/acme/api.git tmp=/tmp/gitraft-NNN
WARN GITLAB_TOKEN unset; GitLab API calls will be unauthenticated (public projects only)
INFO pushing to destination dst=https://gitlab.com/acme/api.git
```

### `--json` (NDJSON, scriptable)

One JSON object per line on **stdout**, with the spinner suppressed. Suitable
for `jq` pipelines and structured log aggregators:

```bash
gitraft migrate --json src dst | jq -r 'select(.level=="ERROR") | .error'
```

Operational errors carry a `\nhint:` preamble. In the wire form that newline
encodes as the `\n` JSON escape — use `jq -r '.error'` to recover the
multi-line form. The exit-error schema is fixed by `cli.ExitErrorEvent`:
`{"level": "ERROR", "msg": "gitraft exited with error", "error": "..."}`.

### `--non-interactive`

Disables the wizard and the spinner. Auto-engaged when stdin or stdout is not
a TTY, when `CI=...` is set, or when `TERM=dumb` — so CI runners never block
on a stale prompt. Cannot be forced on (interactivity is opt-out, not
opt-in).

## Troubleshooting

**`401 Unauthorized` on a private repo**

Check the env var the matching provider expects (see the support matrix
above). Tokens with insufficient scope return 401 even when set — see the
hint that follows each 401 message for the exact scope required.

**`refusing redirect` from a provider**

The repo was likely renamed or moved at the source. Copy the current URL from
the provider's web UI and rerun.

**`Git LFS detected; fetching object content` hangs**

Most likely the source provider gates LFS behind a separate token scope (e.g.,
Bitbucket Cloud requires LFS to be enabled per-repo and within the
workspace's LFS quota). Check the provider's LFS docs; verify the token's
scope.

**`destination appeared during migration; proceeding`**

Benign — `gitraft` raced another `gitraft` (or a manual `git push`) on the
destination, the API surfaced 409/422 "already exists", and gitraft treated
it as success. Continue using the destination as normal.

**Submodule warnings**

`gitraft` v1 mirrors the parent repo only — it preserves submodule
*references* but does not recursively migrate the submodule repositories
themselves. Recursive migration is tracked under
[v2](https://github.com/Ogguz/gitraft/milestones).

## Security

- Tokens are never logged. Every URL written to stderr or `--json` stdout is
  scrubbed of userinfo via the redact package.
- `gitraft` refuses HTTP redirects from provider APIs; a redirect usually
  means the repo was renamed and silently following could mutate a different
  repository than you asked for.
- The release pipeline produces SHA256 checksums, an SBOM (syft), and
  cosign-signed binaries. See the [installation section](#installation)
  for verification commands.

## Roadmap

- **v1.0** — Stable single-binary, all 5 providers, LFS, submodule warnings.
- **v2** — [Recursive submodule migration, resumable migrations, issue
  migration, wiki migration](https://github.com/Ogguz/gitraft/milestones).
- **v3** — [PR/MR migration, releases + binary assets, branch protection
  rules, labels + milestones](https://github.com/Ogguz/gitraft/milestones).

The full development roadmap lives in the project's GitHub milestones and
issue tracker.

## Contributing

Bug reports, feature requests, and pull requests welcome — see
[CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT — see [LICENSE](LICENSE).
