# Git Migration with History

This repository provides tools for moving a Git repository between hosts while
preserving all commits, branches and tags.  A new Python implementation adds a
cross‑platform and easily extensible alternative to the original Bash script.

## Python migration script

```bash
python git_migration.py SOURCE_REPO DEST_REPO [options]
```

### Options

- `--branches BR1,BR2` – only migrate the listed branches (default: all)
- `--tags TAG1,TAG2` – only migrate the listed tags (default: all)
- `--dry-run` – show commands without executing them
- `--cleanup` – remove the temporary mirror after pushing
- `-v/--verbose` – increase log output (use twice for debug)

The script clones the source repository using `--mirror`, optionally filters the
refs to push, and then pushes them to the destination.  Dry‑run mode prints the
commands without performing any network operations.  When `--cleanup` is
specified, the temporary clone is removed on success.

## Legacy Bash script

For reference, the original `git-mirror.sh` script remains in the repository.
