# Contributing to gitraft

Thanks for your interest! Bug reports, feature requests, and PRs are welcome.

## Reporting bugs

Open an issue using the **Bug report** template. The most useful bug reports
include:

- The **exact command** you ran (with tokens redacted; gitraft does this for
  you in logs, but be careful with what you paste).
- The **gitraft version** (`gitraft --version`).
- The **OS and architecture** (`uname -a` on Unix, `systeminfo` on Windows).
- Whether **`git-lfs` is installed** (`git lfs version`) — relevant for any
  bug involving LFS detection or push.
- The **--json output** if it reproduces under `--json` mode (one error JSON
  is more useful than a paragraph of stderr).

## Requesting features

Open an issue using the **Feature request** template. For larger changes,
please describe the use case before implementing — gitraft's scope is
deliberately narrow (mirror migration; nothing about issues, PRs, or wikis
until v2/v3) and we'd hate to reject a PR you spent days on.

## Development setup

```bash
git clone https://github.com/Ogguz/gitraft.git
cd gitraft
go mod download
go build ./cmd/gitraft
go test ./...
```

Go 1.23 or later is required (this matches the `go` directive in
`go.mod`; the CI matrix runs Linux, macOS, and Windows on Go 1.23). Please
run `go test ./...` locally on at least the platform you most recently
changed before opening a PR.

### Lint

```bash
go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.62
golangci-lint run ./...
```

The linter config lives in `.golangci.yml`. CI fails on any new lint
finding; the existing baseline is documented there.

### Run a smoke test

```bash
# Dry-run mode prints commands without making network calls.
./gitraft migrate --dry-run \
  https://github.com/some/repo.git \
  https://gitlab.com/some/repo.git
```

## Project structure

```
cmd/gitraft/         main entry point (signal wiring, exit-error formatting)
internal/cli/        cobra commands, flag wiring, wizard, --json gate
internal/mirror/     clone → detect-LFS → push → push-LFS orchestration
internal/provider/   provider abstraction + 5 implementations
internal/redact/     URL + attribute scrubbing for slog
internal/config/     YAML config loader
```

The provider implementations follow a uniform shape — `Name`, `Matches`,
`ParseRepo`, `RepoExists`, `CreateRepo`, `AuthURL`. New providers should
copy the test scaffolding from `internal/provider/github/github_test.go`,
which covers the standard error paths via `httptest`.

## Testing conventions

- **Behavior, not implementation.** Tests assert on observable outputs (exit
  code, stderr substring, slog field presence) — not on internal call counts
  unless that's the contract being verified.
- **Marker-prefix substrings for hint contracts.** Operational errors carry
  a `\nhint:` preamble; tests assert `strings.Contains(err, "\nhint:")` (with
  newline anchor) so wording can evolve without rewriting the test.
- **httptest for provider HTTP paths.** No real network calls in unit tests.
- **Test seams via package-level function vars.** When a code path needs to
  bypass external state (TTY, git-lfs presence), the production wiring is
  a `var fnName = realFn` that tests can swap. See `isInteractiveFn`,
  `runWizardFn`, `isGitLFSAvailable`, `runLFSLsFiles` for the pattern.
- **Internal-package tests for unexported state.** When external `_test`
  packages can't reach what they need (e.g., overriding `isGitLFSAvailable`
  from outside `package mirror`), add an internal-package test rather than
  exporting the seam. See `lfs_test.go` for the pattern.

## Commit messages

Conventional-ish, but not strict. The release pipeline reads
`feat:` / `fix:` prefixes for changelog grouping; everything else lands in
the "Other" section. Keep the first line under 72 chars; wrap the body at
~80.

## Pull requests

- One topic per PR. Drive-by formatting / lint fixes are welcome but should
  be a separate commit so reviewers can split attention.
- Include test coverage for any user-visible behavior change. Internal
  refactors with a passing existing-test suite are fine without new tests.
- Update the README's Provider Support Matrix or flag table if you add a
  new provider, flag, or env var.
- Run `go test -race ./...` and `golangci-lint run ./...` locally before
  pushing.

## Code of conduct

By participating, you agree to abide by the
[Contributor Covenant](CODE_OF_CONDUCT.md).

## License

By contributing, you agree your contributions will be licensed under the
project's MIT license — see [LICENSE](LICENSE).
