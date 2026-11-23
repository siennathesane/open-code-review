// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package lore reads locally available Lore revisions through the Lore CLI.
package lore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/alibaba/open-code-review/internal/vcs"
)

type Repository struct {
	root  string
	scope string
}

func New(root, scope string) *Repository {
	if scope == "." {
		scope = ""
	}
	return &Repository{root: root, scope: scope}
}

// Lore emits newline-delimited events. Read them incrementally so directory
// dumps need only keep their paths, not all of the revision metadata.
func (r *Repository) events(ctx context.Context, visit func(string, json.RawMessage) error, args ...string) error {
	cmdCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "lore", append([]string{"--offline", "--json", "--no-pager", "--non-interactive"}, args...)...)
	cmd.Dir = r.root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start lore: %w", err)
	}
	dec := json.NewDecoder(stdout)
	var reported string
	completed := false
	for {
		var ev struct {
			TagName string
			Data    json.RawMessage
		}
		err = dec.Decode(&ev)
		if errors.Is(err, io.EOF) {
			err = nil
			break
		}
		if err != nil {
			break
		}
		if ev.TagName == "complete" {
			var result struct {
				Status int
				Error  struct{ Message string }
			}
			if err = json.Unmarshal(ev.Data, &result); err != nil {
				break
			}
			if result.Status == 0 {
				completed = true
			} else {
				reported = result.Error.Message
			}
		} else if visit != nil {
			if err = visit(ev.TagName, ev.Data); err != nil {
				break
			}
		}
	}
	if err != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("decode lore %s: %w", args[0], err)
	}
	if waitErr != nil {
		if reported == "" {
			reported = strings.TrimSpace(stderr.String())
		}
		return fmt.Errorf("lore %s: %w: %s", args[0], waitErr, reported)
	}
	// revision info may emit a subsequent remote lookup failure after the
	// successful local result. The CLI exit status describes the operation.
	if !completed {
		return fmt.Errorf("lore %s did not complete: %s", args[0], reported)
	}
	return nil
}

type revision struct {
	Repository string
	Revision   string
	BranchName string
	Parent     []string
}

func (r *Repository) revision(ctx context.Context, ref string) (revision, error) {
	tag := "repositoryStatusRevision"
	args := []string{"status", "--revision-only"}
	if ref != "" {
		if strings.HasPrefix(ref, "-") {
			return revision{}, fmt.Errorf("invalid Lore revision %q", ref)
		}
		tag, args = "revisionInfo", []string{"revision", "info", ref}
	}
	var rev revision
	err := r.events(ctx, func(t string, data json.RawMessage) error {
		if t == tag {
			return json.Unmarshal(data, &rev)
		}
		return nil
	}, args...)
	if err == nil && (rev.Repository == "" || rev.Revision == "") {
		err = fmt.Errorf("lore returned no revision information for %q", ref)
	}
	return rev, err
}

const zeroRevision = "0000000000000000000000000000000000000000000000000000000000000000"

func (r *Repository) CurrentBranch(ctx context.Context) (string, error) {
	rev, err := r.revision(ctx, "")
	return rev.BranchName, err
}

// ResolveReview pins the requested endpoints once, before any diff or context
// read. A --from/--to request flattens every revision between the endpoints
// into a single changeset. When both endpoints name branches, Lore's own
// common-ancestor computation (`branch diff`) selects the comparison base;
// otherwise the endpoints are revisions (numbers or signatures) compared
// directly, since Lore revision numbers are repository-global. A single
// revision compares against its first parent.
func (r *Repository) ResolveReview(ctx context.Context, spec vcs.ReviewSpec) (vcs.ResolvedReview, error) {
	var out vcs.ResolvedReview
	if err := spec.Validate(); err != nil {
		return out, err
	}
	if spec.Mode == vcs.MergeBase {
		return r.resolveMergeBase(ctx, spec)
	}
	rev, err := r.revision(ctx, spec.Head)
	if err != nil {
		return out, err
	}
	out = vcs.ResolvedReview{Provider: "lore", RepositoryID: rev.Repository, Spec: spec, Base: rev.Revision}
	if spec.Mode != vcs.Workspace {
		out.Head, out.Content.Revision = rev.Revision, rev.Revision
		if spec.Mode == vcs.Commit {
			if len(rev.Parent) == 0 {
				return out, fmt.Errorf("lore revision %q has no parent metadata", rev.Revision)
			}
			out.Base = rev.Parent[0]
		} else {
			base, err := r.revision(ctx, spec.Base)
			if err != nil {
				return out, err
			}
			out.Base = base.Revision
		}
	}
	if out.Base == zeroRevision {
		out.Base, out.BaseIsEmpty = "", true
	}
	return out, nil
}

// resolveMergeBase resolves a --from/--to pair. The endpoints are classified
// first, by listing branches: when both names are branches, `lore branch diff`
// reports their common ancestor and the source branch head, and every failure
// of that computation surfaces — it is never substituted with a different
// comparison. A pair with no common ancestor is an error, matching the Git
// provider. When at least one name is not a branch, both endpoints resolve as
// revisions (numbers or signatures) and compare directly, so an explicit fork
// revision keeps working.
func (r *Repository) resolveMergeBase(ctx context.Context, spec vcs.ReviewSpec) (vcs.ResolvedReview, error) {
	var out vcs.ResolvedReview
	for _, name := range []string{spec.Base, spec.Head} {
		if strings.HasPrefix(name, "-") {
			return out, fmt.Errorf("invalid Lore name %q", name)
		}
	}
	branches, err := r.branchNames(ctx)
	if err != nil {
		// Without the branch listing the classification cannot happen at all, so
		// neither resolution path is justified; report instead of guessing.
		return out, fmt.Errorf("resolve Lore range %q..%q: %w", spec.Base, spec.Head, err)
	}
	if branches[spec.Base] && branches[spec.Head] {
		return r.resolveBranchRange(ctx, spec)
	}
	head, err := r.revision(ctx, spec.Head)
	if err != nil {
		return out, fmt.Errorf("resolve Lore range %q..%q: %w", spec.Base, spec.Head, err)
	}
	base, err := r.revision(ctx, spec.Base)
	if err != nil {
		return out, fmt.Errorf("resolve Lore range %q..%q: %w", spec.Base, spec.Head, err)
	}
	out = vcs.ResolvedReview{
		Provider:     "lore",
		RepositoryID: head.Repository,
		Spec:         spec,
		Base:         base.Revision,
		Head:         head.Revision,
		Content:      vcs.ContentRef{Revision: head.Revision},
	}
	if out.Base == zeroRevision {
		out.Base, out.BaseIsEmpty = "", true
	}
	return out, nil
}

// branchNames returns the repository's branch names from `lore branch list`.
func (r *Repository) branchNames(ctx context.Context) (map[string]bool, error) {
	names := map[string]bool{}
	err := r.events(ctx, func(tag string, data json.RawMessage) error {
		if tag != "branchListEntry" {
			return nil
		}
		var entry struct {
			Name string
		}
		if err := json.Unmarshal(data, &entry); err != nil {
			return err
		}
		names[entry.Name] = true
		return nil
	}, "branch", "list")
	if err != nil {
		return nil, err
	}
	return names, nil
}

// resolveBranchRange resolves two branch names through Lore's common-ancestor
// computation. The request then reviews exactly the branch's own revisions:
// common ancestor as base, source branch head as head.
func (r *Repository) resolveBranchRange(ctx context.Context, spec vcs.ReviewSpec) (vcs.ResolvedReview, error) {
	var out vcs.ResolvedReview
	var begin struct {
		SourceRevision string
		BaseRevision   string
	}
	err := r.events(ctx, func(tag string, data json.RawMessage) error {
		if tag == "branchDiffBegin" {
			return json.Unmarshal(data, &begin)
		}
		return nil
	}, "branch", "diff", spec.Base, "--source", spec.Head)
	if err != nil {
		return out, fmt.Errorf("resolve Lore branches %q..%q: %w", spec.Base, spec.Head, err)
	}
	switch {
	case begin.SourceRevision == "" || begin.BaseRevision == "":
		return out, fmt.Errorf("resolve Lore branches %q..%q: lore reported no branch comparison", spec.Base, spec.Head)
	case begin.BaseRevision == zeroRevision || begin.SourceRevision == zeroRevision:
		return out, fmt.Errorf("resolve Lore branches %q..%q: branches have no common ancestor", spec.Base, spec.Head)
	}
	out = vcs.ResolvedReview{
		Provider: "lore",
		Spec:     spec,
		Base:     begin.BaseRevision,
		Head:     begin.SourceRevision,
		Content:  vcs.ContentRef{Revision: begin.SourceRevision},
	}
	// branchDiffBegin carries branch IDs, not the repository ID the
	// manifest records, so ask the status event for it.
	rev, revErr := r.revision(ctx, "")
	if revErr != nil {
		return out, revErr
	}
	out.RepositoryID = rev.Repository
	return out, nil
}
