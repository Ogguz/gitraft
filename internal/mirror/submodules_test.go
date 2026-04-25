package mirror

import (
	"context"
	"path/filepath"
	"testing"
)

func TestParseGitmodules_Standard(t *testing.T) {
	content := `[submodule "lib/foo"]
	path = lib/foo
	url = https://github.com/x/y.git
[submodule "lib/bar"]
	path = lib/bar
	url = https://github.com/a/b.git
`
	mods := parseGitmodules(content)
	if len(mods) != 2 {
		t.Fatalf("got %d mods; want 2: %+v", len(mods), mods)
	}
	if mods[0].Path != "lib/foo" || mods[0].URL != "https://github.com/x/y.git" {
		t.Errorf("mod 0 = %+v", mods[0])
	}
	if mods[1].Path != "lib/bar" || mods[1].URL != "https://github.com/a/b.git" {
		t.Errorf("mod 1 = %+v", mods[1])
	}
}

func TestParseGitmodules_Empty(t *testing.T) {
	if mods := parseGitmodules(""); len(mods) != 0 {
		t.Errorf("expected empty result; got %+v", mods)
	}
}

func TestParseGitmodules_CommentsAndBlankLines(t *testing.T) {
	content := `# top comment

[submodule "deps/x"]
	# inline comment
	path = deps/x
	url = git@github.com:org/x.git

; semicolon comment
[submodule "deps/y"]
	path = deps/y
	url = https://example.com/y.git
`
	mods := parseGitmodules(content)
	if len(mods) != 2 {
		t.Fatalf("got %d mods; want 2", len(mods))
	}
}

func TestParseGitmodules_QuotedValues(t *testing.T) {
	content := `[submodule "x"]
	path = "lib/x"
	url = "https://example.com/x.git"
`
	mods := parseGitmodules(content)
	if len(mods) != 1 {
		t.Fatalf("got %d mods; want 1", len(mods))
	}
	if mods[0].Path != "lib/x" || mods[0].URL != "https://example.com/x.git" {
		t.Errorf("got %+v", mods[0])
	}
}

func TestParseGitmodules_PartialEntrySkippedWithoutPath(t *testing.T) {
	content := `[submodule "incomplete"]
	url = https://example.com/x.git
`
	mods := parseGitmodules(content)
	if len(mods) != 0 {
		t.Errorf("entry without path must be dropped; got %+v", mods)
	}
}

func TestParseGitmodules_UnknownKeysIgnored(t *testing.T) {
	content := `[submodule "x"]
	path = lib/x
	url = https://example.com/x.git
	branch = main
	update = checkout
	ignore = none
	custom = whatever
`
	mods := parseGitmodules(content)
	if len(mods) != 1 || mods[0].Path != "lib/x" {
		t.Errorf("expected single mod with path lib/x; got %+v", mods)
	}
}

func TestListSubmodules_NoFile(t *testing.T) {
	internalRequireGit(t)
	bare := internalMakeBareRepo(t, internalSeedReadme)
	mods, _, err := listSubmodules(context.Background(), bare)
	if err != nil {
		t.Fatal(err)
	}
	if mods != nil {
		t.Errorf("expected nil for repo without .gitmodules; got %+v", mods)
	}
}

func TestListSubmodules_EmptyDirReturnsNil(t *testing.T) {
	mods, _, err := listSubmodules(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if mods != nil {
		t.Errorf("expected nil for empty dir; got %+v", mods)
	}
}

func TestListSubmodules_ReadsFromBareRepoHEAD(t *testing.T) {
	internalRequireGit(t)
	bare := internalMakeBareRepo(t, internalSeedReadme, internalSeedGitmodules)
	mods, _, err := listSubmodules(context.Background(), bare)
	if err != nil {
		t.Fatal(err)
	}
	if len(mods) != 1 {
		t.Fatalf("got %d mods; want 1", len(mods))
	}
	if mods[0].Path != "vendor/dep" {
		t.Errorf("path = %q", mods[0].Path)
	}
}

func TestParseGitmodules_SectionNameWithSpace(t *testing.T) {
	content := `[submodule "lib with space"]
	path = lib/space
	url = https://example.com/x.git
`
	mods := parseGitmodules(content)
	if len(mods) != 1 || mods[0].Path != "lib/space" {
		t.Errorf("section name with space should still parse; got %+v", mods)
	}
}

func TestParseGitmodules_DuplicateKeysLastWins(t *testing.T) {
	content := `[submodule "x"]
	path = lib/old
	path = lib/new
	url = https://example.com/old.git
	url = https://example.com/new.git
`
	mods := parseGitmodules(content)
	if len(mods) != 1 {
		t.Fatalf("got %d mods", len(mods))
	}
	if mods[0].Path != "lib/new" || mods[0].URL != "https://example.com/new.git" {
		t.Errorf("expected last-write-wins; got %+v", mods[0])
	}
}

func TestParseGitmodules_EmptyValueDropsEntry(t *testing.T) {
	content := `[submodule "x"]
	path =
	url = https://example.com/x.git
`
	mods := parseGitmodules(content)
	if len(mods) != 0 {
		t.Errorf("empty path should drop the entry; got %+v", mods)
	}
}


// internalSeedGitmodules adds a .gitmodules file (without actually configuring
// a real submodule) so listSubmodules can parse it. Submodule add would
// require network, which we avoid in unit tests.
func internalSeedGitmodules(t *testing.T, work string) {
	t.Helper()
	content := "[submodule \"vendor/dep\"]\n\tpath = vendor/dep\n\turl = https://example.com/dep.git\n"
	internalWriteFile(t, filepath.Join(work, ".gitmodules"), content)
	internalRunGit(t, work, "add", ".gitmodules")
	internalRunGit(t, work, "commit", "-m", "add gitmodules")
}
