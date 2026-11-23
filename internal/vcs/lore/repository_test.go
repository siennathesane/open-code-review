// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package lore

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/model"
	"github.com/alibaba/open-code-review/internal/vcs"
)

const stubLoreScript = `#!/bin/sh
while [ $# -gt 0 ]; do
	case $1 in
	--*) shift ;;
	*) break ;;
	esac
done
case "$1 $2" in
"branch diff") printf '%s\n' "$LORE_STUB_BRANCH_DIFF" ;;
"branch list") printf '%s\n' "$LORE_STUB_BRANCH_LIST" ;;
"status --revision-only") printf '%s\n' "$LORE_STUB_STATUS" ;;
"revision info")
	printf '{"tagName":"revisionInfo","data":{"repository":"repo-id","revision":"sig-%s","parent":[]}}\n' "$3"
	printf '%s\n' '{"tagName":"complete","data":{"status":0}}' ;;
esac
exit 0
`

const (
	stubBranchListBoth = `{"tagName":"branchListEntry","data":{"name":"main"}}
{"tagName":"branchListEntry","data":{"name":"feature"}}
{"tagName":"complete","data":{"status":0}}`
	stubBranchListMain = `{"tagName":"branchListEntry","data":{"name":"main"}}
{"tagName":"complete","data":{"status":0}}`
	stubStatus = `{"tagName":"repositoryStatusRevision","data":{"repository":"repo-id","branchName":"feature","revision":"cur"}}
{"tagName":"complete","data":{"status":0}}`
)

// stubLore installs a fake lore executable whose NDJSON responses come from
// environment variables, one per subcommand, so event-payload shapes are
// pinned deterministically without an installed CLI.
func stubLore(t *testing.T, responses map[string]string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the lore stub is a POSIX shell script")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "lore")
	if err := os.WriteFile(script, []byte(stubLoreScript), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range responses {
		t.Setenv(name, value)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestResolveMergeBaseClassification(t *testing.T) {
	const happyBranchDiff = `{"tagName":"branchDiffBegin","data":{"sourceRevision":"src-head","baseRevision":"base-sig"}}
{"tagName":"complete","data":{"status":0}}`
	branchRange := vcs.ReviewSpec{Mode: vcs.MergeBase, Base: "main", Head: "feature"}

	t.Run("engages the branch comparison", func(t *testing.T) {
		stubLore(t, map[string]string{
			"LORE_STUB_BRANCH_DIFF": happyBranchDiff,
			"LORE_STUB_BRANCH_LIST": stubBranchListBoth,
			"LORE_STUB_STATUS":      stubStatus,
		})
		out, err := New(t.TempDir(), ".").ResolveReview(t.Context(), branchRange)
		if err != nil {
			t.Fatal(err)
		}
		if out.Base != "base-sig" || out.Head != "src-head" || out.Content.Revision != "src-head" || out.RepositoryID != "repo-id" || out.BaseIsEmpty {
			t.Fatalf("branch comparison endpoints: %+v", out)
		}
		if out.Spec != branchRange {
			t.Fatalf("spec not preserved: %+v", out.Spec)
		}
	})

	t.Run("refuses unrelated histories", func(t *testing.T) {
		stubLore(t, map[string]string{
			"LORE_STUB_BRANCH_DIFF": `{"tagName":"branchDiffBegin","data":{"sourceRevision":"src-head","baseRevision":"0000000000000000000000000000000000000000000000000000000000000000"}}
{"tagName":"complete","data":{"status":0}}`,
			"LORE_STUB_BRANCH_LIST": stubBranchListBoth,
			"LORE_STUB_STATUS":      stubStatus,
		})
		_, err := New(t.TempDir(), ".").ResolveReview(t.Context(), branchRange)
		if err == nil || !strings.Contains(err.Error(), "no common ancestor") {
			t.Fatalf("unrelated histories = %v, want a no-common-ancestor error", err)
		}
	})

	t.Run("surfaces branch diff failures", func(t *testing.T) {
		stubLore(t, map[string]string{
			"LORE_STUB_BRANCH_DIFF": `{"tagName":"complete","data":{"status":87,"error":{"message":"branch lookup failed"}}}`,
			"LORE_STUB_BRANCH_LIST": stubBranchListBoth,
		})
		_, err := New(t.TempDir(), ".").ResolveReview(t.Context(), branchRange)
		if err == nil || !strings.Contains(err.Error(), "resolve Lore branches") || !strings.Contains(err.Error(), "branch lookup failed") {
			t.Fatalf("branch diff failure = %v, want a surfaced branch error", err)
		}
	})

	t.Run("non-branch names resolve as revisions", func(t *testing.T) {
		stubLore(t, map[string]string{
			"LORE_STUB_BRANCH_LIST": stubBranchListMain,
			"LORE_STUB_STATUS":      stubStatus,
		})
		out, err := New(t.TempDir(), ".").ResolveReview(t.Context(), vcs.ReviewSpec{Mode: vcs.MergeBase, Base: "1", Head: "feature"})
		if err != nil {
			t.Fatal(err)
		}
		if out.Base != "sig-1" || out.Head != "sig-feature" || out.Content.Revision != "sig-feature" || out.RepositoryID != "repo-id" {
			t.Fatalf("revision fallback endpoints: %+v", out)
		}
	})
}

func TestWorkspaceFileBoundary(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ok.go"), []byte("inside"), 0600); err != nil {
		t.Fatal(err)
	}
	repo := New(root, ".")
	for _, name := range []string{"../outside", ".lore/state", "nested/.lore/config", "/outside", ".", `nested\..\outside`} {
		if err := repo.ReadFile(t.Context(), vcs.ContentRef{}, name, func(io.Reader) error { t.Fatal("unexpected read"); return nil }); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	if err := New(root, ".lore").ReadFile(t.Context(), vcs.ContentRef{}, "state", func(io.Reader) error { return nil }); err == nil {
		t.Fatal("accepted an administrative scope")
	}
	sentinel := errors.New("stop")
	if err := repo.ReadFile(t.Context(), vcs.ContentRef{}, "ok.go", func(io.Reader) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := repo.ReadFile(ctx, vcs.ContentRef{}, "ok.go", nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestOfflineRepository(t *testing.T) {
	binary, err := exec.LookPath("lore")
	if err != nil {
		t.Skip("install Lore CLI to run the offline integration test")
	}
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), binary, append([]string{"--offline", "--non-interactive"}, args...)...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("lore %v: %v: %s", args, err, out)
		}
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run("repository", "create", "review-test")
	repo := New(root, ".")
	changes := func(spec vcs.ReviewSpec) map[string]model.Diff {
		t.Helper()
		rev, err := repo.ResolveReview(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		set, err := repo.Changes(t.Context(), rev)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]model.Diff{}
		for _, d := range set.Included {
			p := d.NewPath
			if d.IsDeleted {
				p = d.OldPath
			}
			out[p] = d
		}
		return out
	}
	workspace := vcs.ReviewSpec{Mode: vcs.Workspace}
	write("main.go", "package first\n")
	if d := changes(workspace)["main.go"]; !d.IsNew || d.Insertions != 1 {
		t.Fatalf("unborn: %+v", d)
	}
	run("stage", "--scan", ".")
	run("commit", "first")
	write("main.go", "package second\n")
	write("empty.go", "")
	run("stage", "--scan", ".")
	run("commit", "second")
	if err := os.Remove(filepath.Join(root, "main.go")); err != nil {
		t.Fatal(err)
	}
	write("local.go", "package local\n")
	ws := changes(workspace)
	if !ws["main.go"].IsDeleted || !ws["local.go"].IsNew {
		t.Fatalf("workspace: %+v", ws)
	}
	for _, spec := range []vcs.ReviewSpec{{Mode: vcs.Commit, Head: "2"}, {Mode: vcs.TwoRevisions, Base: "1", Head: "2"}} {
		got := changes(spec)
		if d := got["main.go"]; d.Insertions != 1 || d.Deletions != 1 || d.NewFileContent != "package second\n" {
			t.Fatalf("%s: %+v", spec.Mode, d)
		}
		if !got["empty.go"].IsNew {
			t.Fatalf("missing empty addition: %+v", got)
		}
	}
	rootCommit := changes(vcs.ReviewSpec{Mode: vcs.Commit, Head: "1"})
	if d := rootCommit["main.go"]; !d.IsNew || !strings.Contains(d.Diff, "+package first") {
		t.Fatalf("first revision: %+v", d)
	}
	// A range review flattens every revision between the endpoints into one
	// changeset: first..third spans two commits, so it carries the third
	// content of main.go plus both files added along the way, including the
	// empty addition the line diff alone would miss.
	write("main.go", "package third\n")
	run("stage", "--scan", ".")
	run("commit", "third")
	for _, spec := range []vcs.ReviewSpec{
		{Mode: vcs.MergeBase, Base: "1", Head: "3"},
		{Mode: vcs.TwoRevisions, Base: "1", Head: "3"},
	} {
		rng := changes(spec)
		if d := rng["main.go"]; d.IsNew || d.Insertions != 1 || d.Deletions != 1 || d.NewFileContent != "package third\n" {
			t.Fatalf("%s main.go: %+v", spec.Mode, d)
		}
		if !rng["empty.go"].IsNew {
			t.Fatalf("%s missing empty addition: %+v", spec.Mode, rng)
		}
		if d := rng["local.go"]; !d.IsNew || d.NewFileContent != "package local\n" {
			t.Fatalf("%s local.go: %+v", spec.Mode, d)
		}
	}
	files, err := repo.ListFiles(t.Context(), vcs.ContentRef{})
	if err != nil || !slices.Equal(files, []string{"empty.go", "local.go", "main.go"}) {
		t.Fatalf("workspace files: %v, %v", files, err)
	}
	if err := repo.ReadFile(t.Context(), vcs.ContentRef{Revision: "1"}, "local.go", func(io.Reader) error { return nil }); err == nil {
		t.Fatal("historical read returned a workspace-only file")
	}
	// A branch pair resolves through Lore's common-ancestor computation: the
	// review covers exactly the branch's own revisions, not main's.
	run("branch", "create", "feature")
	write("extra.go", "package extra\n")
	run("stage", "--scan", ".")
	run("commit", "extra")
	branched := changes(vcs.ReviewSpec{Mode: vcs.MergeBase, Base: "main", Head: "feature"})
	if d := branched["extra.go"]; !d.IsNew || d.NewFileContent != "package extra\n" {
		t.Fatalf("branch review extra.go: %+v", d)
	}
	if _, found := branched["main.go"]; found {
		t.Fatalf("branch review included main's revisions: %+v", branched)
	}
	// Historical range reads stay pinned to the head revision even after the
	// working copy moves on.
	write("main.go", "package dirty workspace\n")
	if d := changes(vcs.ReviewSpec{Mode: vcs.MergeBase, Base: "1", Head: "3"})["main.go"]; d.NewFileContent != "package third\n" {
		t.Fatalf("dirty workspace leaked into range read: %+v", d)
	}
}
