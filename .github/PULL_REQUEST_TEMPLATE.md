<!--
Thanks for the PR! A few things that make review faster:

- One topic per PR. Drive-by lint/format fixes are welcome but ideally as a
  separate commit so reviewers can split attention.
- Run `go test -race ./...` and `golangci-lint run ./...` locally before
  pushing.
- Update the README's provider matrix or flag table if you added a flag,
  env var, or provider.
-->

## What this changes

<!-- One or two sentences. Why is this change needed? -->

## How it works

<!-- Brief tour of the implementation. Skip if obvious from the diff. -->

## Testing

<!--
What did you do to convince yourself this works? Tests, manual smoke,
existing CI? If this changes user-visible behavior, a test is expected.
-->

- [ ] `go test -race ./...` passes locally
- [ ] `golangci-lint run ./...` reports no new findings
- [ ] Updated README / docs for any new flag, env var, or provider
- [ ] Added or updated tests for user-visible behavior

## Linked issues

<!-- "Fixes #123" or "Refs #456" — leave blank for refactors with no issue. -->
