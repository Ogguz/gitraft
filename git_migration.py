import argparse
import logging
import os
import shutil
import subprocess
import tempfile
from typing import List


def run(cmd: List[str], dry_run: bool) -> None:
    """Run a command, respecting dry-run mode."""
    if dry_run:
        logging.info("DRY-RUN: %s", " ".join(cmd))
        return
    logging.debug("Running: %s", " ".join(cmd))
    subprocess.run(cmd, check=True)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Migrate one git repository to another while preserving history."
    )
    parser.add_argument("source", help="URL or path of the source repository")
    parser.add_argument("destination", help="URL or path of the destination repository")
    parser.add_argument(
        "--branches",
        help="Comma separated list of branches to migrate (default: all)",
    )
    parser.add_argument(
        "--tags",
        help="Comma separated list of tags to migrate (default: all)",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="Show what would be executed without making changes",
    )
    parser.add_argument(
        "--cleanup",
        action="store_true",
        help="Remove temporary clone after completion",
    )
    parser.add_argument(
        "-v",
        "--verbose",
        action="count",
        default=0,
        help="Increase logging verbosity",
    )
    return parser.parse_args()


def setup_logging(level: int) -> None:
    if level >= 2:
        logging.basicConfig(level=logging.DEBUG, format="%(levelname)s: %(message)s")
    elif level == 1:
        logging.basicConfig(level=logging.INFO, format="%(levelname)s: %(message)s")
    else:
        logging.basicConfig(level=logging.WARNING, format="%(levelname)s: %(message)s")


def build_refspec(prefix: str, items: List[str]) -> List[str]:
    return [f"+refs/{prefix}/{name}:refs/{prefix}/{name}" for name in items]


def migrate_repo(args: argparse.Namespace) -> None:
    tmpdir = tempfile.mkdtemp(prefix="git-migrate-")
    logging.info("Cloning source repository ...")
    run(["git", "clone", "--mirror", args.source, tmpdir], args.dry_run)

    os.chdir(tmpdir)
    refspec: List[str] = []
    if args.branches:
        branches = [b.strip() for b in args.branches.split(",") if b.strip()]
        refspec.extend(build_refspec("heads", branches))
    if args.tags:
        tags = [t.strip() for t in args.tags.split(",") if t.strip()]
        refspec.extend(build_refspec("tags", tags))

    push_cmd = ["git", "push", args.destination]
    if refspec:
        push_cmd.extend(refspec)
    else:
        push_cmd.append("--mirror")

    logging.info("Pushing to destination ...")
    run(push_cmd, args.dry_run)

    if args.cleanup and not args.dry_run:
        logging.info("Cleaning up %s", tmpdir)
        os.chdir("/")
        shutil.rmtree(tmpdir)
    else:
        logging.info("Temporary clone located at %s", tmpdir)


def main() -> None:
    args = parse_args()
    setup_logging(args.verbose)
    migrate_repo(args)


if __name__ == "__main__":
    main()
