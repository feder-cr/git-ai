package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/changelog/member"
	"github.com/feder-cr/git-ai/internal/graft/workspace"
	"github.com/feder-cr/git-ai/sdk"
)

const (
	defaultAuthorName  = "graft"
	defaultAuthorEmail = "graft@localhost"
	defaultStorage     = "default"
	shortOIDLength     = 12
)

func cmdClone(ctx context.Context, out io.Writer, o *options, args []string) error {
	if len(args) != 3 {
		return usagef("clone requires ADDR REPO DIR")
	}
	address, repo, dir := args[0], args[1], args[2]
	storage := pick(o.storage, defaultStorage)
	backend, err := backendFactory(ctx, target{Address: address, Token: o.token, Storage: storage, Repo: repo, User: o.user})
	if err != nil {
		return err
	}
	ws, err := workspace.Clone(ctx, dir, backend, workspace.CloneOptions{
		Address: address,
		Storage: storage,
		Repo:    repo,
		Branch:  o.branch,
		Sparse:  splitList(o.sparse),
	})
	if err != nil {
		return err
	}
	warnSkipped(o, ws)
	s := ws.Summary()
	fmt.Fprintf(out, "cloned %s (%s) into %s: %d files at %s\n", repo, s.Branch, dir, s.Files, shortOID(s.Base))
	return nil
}

// warnSkipped reports the repository paths that the last sync of ws did not
// materialize, because they are reserved or unsafe file names on this system.
func warnSkipped(o *options, ws *workspace.Workspace) {
	if o.stderr == nil {
		return
	}
	for _, skipped := range ws.Skipped() {
		fmt.Fprintf(o.stderr, "graft: warning: not materialized: %s\n", skipped)
	}
}

func cmdStatus(ctx context.Context, out io.Writer, o *options, args []string) error {
	if len(args) != 0 {
		return usagef("status takes no arguments")
	}
	ws, err := openWorkspace(ctx, o)
	if err != nil {
		return err
	}
	changes, err := ws.Status()
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		fmt.Fprintln(out, "nothing changed")
	}
	for _, c := range changes {
		fmt.Fprintf(out, "%s %s\n", c.Kind, c.Path)
	}
	return nil
}

func cmdDiff(ctx context.Context, out io.Writer, o *options, args []string) error {
	if len(args) != 0 {
		return usagef("diff takes no arguments")
	}
	ws, err := openWorkspace(ctx, o)
	if err != nil {
		return err
	}
	diff, err := ws.Diff()
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, diff)
	return err
}

func cmdCommit(ctx context.Context, out io.Writer, o *options, args []string) error {
	if len(args) != 0 {
		return usagef("commit takes flags only: commit -m MESSAGE [--land BRANCH]")
	}
	if strings.TrimSpace(o.message) == "" {
		return usagef("commit requires -m MESSAGE")
	}
	// Resolution happens while landing, so it is meaningless without a landing.
	if o.resolve && o.land == "" {
		return usagef("commit --resolve requires --land BRANCH")
	}
	if err := checkStrategy(o.strategy); err != nil {
		return err
	}
	author, err := parseAuthor(o.author)
	if err != nil {
		return err
	}
	ws, err := openWorkspace(ctx, o)
	if err != nil {
		return err
	}
	res, err := ws.Commit(ctx, workspace.CommitRequest{
		Message:    o.message,
		Author:     author,
		User:       o.user,
		LandBranch: o.land,
		Strategy:   o.strategy,
		Resolve:    o.resolve,
	})
	warnSkipped(o, ws)
	if err != nil {
		return err
	}
	if res.Landed {
		fmt.Fprintf(out, "committed %s (%d changes) and landed it on %s%s%s\n", shortOID(res.Commit), res.Changes, o.land,
			resolvedNote(res.ResolvedPaths), batchNote(res.BatchSize, res.BatchPosition, res.Attempts))
		return nil
	}
	fmt.Fprintf(out, "committed %s (%d changes) on %s; run graft land to land it\n", shortOID(res.Commit), res.Changes, res.Branch)
	return nil
}

func cmdLand(ctx context.Context, out io.Writer, o *options, args []string) error {
	if len(args) > 1 {
		return usagef("land takes at most one BRANCH")
	}
	branch := ""
	if len(args) == 1 {
		branch = args[0]
	}
	if o.abort {
		if branch != "" {
			return usagef("land --abort takes no BRANCH")
		}
		ws, err := openWorkspace(ctx, o)
		if err != nil {
			return err
		}
		if err := ws.Abort(ctx); err != nil {
			return err
		}
		fmt.Fprintln(out, "dropped the pending commit; local changes are kept (graft revert restores files)")
		return nil
	}
	if err := checkStrategy(o.strategy); err != nil {
		return err
	}
	committer, err := parseAuthor(o.author)
	if err != nil {
		return err
	}
	if o.conflictsDir != "" && !o.resolve {
		return usagef("land --conflicts-dir requires --resolve")
	}
	if o.resolved != "" && (o.resolve || o.conflictsDir != "") {
		return usagef("land --resolved takes neither --resolve nor --conflicts-dir")
	}
	ws, err := openWorkspace(ctx, o)
	if err != nil {
		return err
	}
	var res workspace.LandResult
	if o.resolved != "" {
		resolution, rerr := readResolution(o.resolved)
		if rerr != nil {
			return rerr
		}
		res, err = ws.LandResolution(ctx, branch, committer, resolution)
	} else {
		res, err = ws.LandResolving(ctx, branch, o.strategy, committer, o.resolve)
	}
	warnSkipped(o, ws)
	if err != nil {
		// A land that asked for resolution fails with the conflict package.
		if pkg, ok := sdk.LandConflict(err); ok {
			printConflict(out, pkg)
			if o.conflictsDir != "" {
				if werr := writeConflicts(ctx, ws, pkg, o.conflictsDir, out); werr != nil {
					return fmt.Errorf("%w (and writing the conflicts failed: %v)", err, werr)
				}
				fmt.Fprintf(out, "wrote the conflicts to %s: edit the files, then run graft land --resolved %s\n", o.conflictsDir, o.conflictsDir)
			}
		}
		return err
	}
	if res.AlreadyLanded {
		fmt.Fprintf(out, "already landed at %s\n", shortOID(res.Commit))
		return nil
	}
	fmt.Fprintf(out, "landed at %s%s%s\n", shortOID(res.Commit), resolvedNote(res.ResolvedPaths), batchNote(res.BatchSize, res.BatchPosition, res.Attempts))
	return nil
}

// batchNote describes the land batch a change was part of, such as " (batch 3/17,
// 1 attempt)", or returns the empty string when the change landed alone. Zero
// attempts means the server did not report them, and then they are left out.
func batchNote(size, position, attempts int32) string {
	if size <= 1 {
		return ""
	}
	if attempts <= 0 {
		return fmt.Sprintf(" (batch %d/%d)", position, size)
	}
	unit := "attempts"
	if attempts == 1 {
		unit = "attempt"
	}
	return fmt.Sprintf(" (batch %d/%d, %d %s)", position, size, attempts, unit)
}

// resolvedNote describes the files the server resolved while landing, or returns
// the empty string when none were. The paths are escaped with displayText.
func resolvedNote(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	return fmt.Sprintf(" (resolved %d conflicting files: %s)", len(paths), displayList(paths))
}

func cmdUpdate(ctx context.Context, out io.Writer, o *options, args []string) error {
	if len(args) != 0 {
		return usagef("update takes no arguments")
	}
	ws, err := openWorkspace(ctx, o)
	if err != nil {
		return err
	}
	res, err := ws.Update(ctx)
	warnSkipped(o, ws)
	if err != nil {
		return err
	}
	if res.Old == res.New {
		fmt.Fprintf(out, "already up to date at %s\n", shortOID(res.New))
		return nil
	}
	fmt.Fprintf(out, "updated %s -> %s\n", shortOID(res.Old), shortOID(res.New))
	return nil
}

func cmdRevert(ctx context.Context, out io.Writer, o *options, args []string) error {
	ws, err := openWorkspace(ctx, o)
	if err != nil {
		return err
	}
	if err := ws.Revert(ctx, args); err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Fprintln(out, "reverted all local changes")
		return nil
	}
	fmt.Fprintf(out, "reverted %s\n", strings.Join(args, ", "))
	return nil
}

func cmdSparse(ctx context.Context, out io.Writer, o *options, args []string) error {
	ws, err := openWorkspace(ctx, o)
	if err != nil {
		return err
	}
	err = ws.Sparse(ctx, args)
	warnSkipped(o, ws)
	if err != nil {
		return err
	}
	sparse := ws.Summary().Sparse
	if len(sparse) == 0 {
		fmt.Fprintln(out, "sparse: whole repository")
		return nil
	}
	fmt.Fprintf(out, "sparse: %s\n", strings.Join(sparse, ", "))
	return nil
}

func cmdLog(ctx context.Context, out io.Writer, o *options, args []string) error {
	if len(args) != 0 {
		return usagef("log takes no arguments")
	}
	backend, _, err := remoteFor(ctx, o)
	if err != nil {
		return err
	}
	changesets, err := backend.Changesets(ctx, o.after, o.limit)
	if err != nil {
		return err
	}
	for _, c := range changesets {
		if err := emitChangeset(out, o, c); err != nil {
			return err
		}
	}
	return nil
}

func cmdWatch(ctx context.Context, out io.Writer, o *options, args []string) error {
	if len(args) != 0 {
		return usagef("watch takes no arguments")
	}
	backend, _, err := remoteFor(ctx, o)
	if err != nil {
		return err
	}
	changesets, errs := backend.WatchChangesets(ctx, o.after)
	for {
		select {
		case c, ok := <-changesets:
			if !ok {
				// A backend reports a failure on errs before it closes changesets,
				// and select may see the closed changesets first.
				return drainWatchError(ctx, errs)
			}
			if err := emitChangeset(out, o, c); err != nil {
				return err
			}
		case watchErr, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if watchErr != nil {
				return watchErr
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// drainWatchError returns the failure of a watch whose changesets channel has
// closed. Backends send a failure on errs before they close the changesets
// channel and then close errs, so the result does not depend on which of the two
// ready channels a select picked. A nil errs was already drained.
func drainWatchError(ctx context.Context, errs <-chan error) error {
	if errs == nil {
		return nil
	}
	for {
		select {
		case err, ok := <-errs:
			if !ok {
				return nil
			}
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func cmdBlame(ctx context.Context, out io.Writer, o *options, args []string) error {
	if len(args) != 1 {
		return usagef("blame requires PATH")
	}
	path, err := workspace.ValidatePath(args[0])
	if err != nil {
		return usagef("%v", err)
	}
	backend, meta, err := remoteFor(ctx, o)
	if err != nil {
		return err
	}
	lines, err := backend.Blame(ctx, pick(o.revision, meta.Branch), path)
	if err != nil {
		return err
	}
	for _, line := range lines {
		fmt.Fprintf(out, "%s %5d) %s\n", shortOID(line.CommitID), line.LineNumber, line.Content)
	}
	return nil
}

func cmdHistory(ctx context.Context, out io.Writer, o *options, args []string) error {
	if len(args) != 1 {
		return usagef("history requires PATH")
	}
	path, err := workspace.ValidatePath(args[0])
	if err != nil {
		return usagef("%v", err)
	}
	backend, meta, err := remoteFor(ctx, o)
	if err != nil {
		return err
	}
	commits, err := backend.PathHistory(ctx, pick(o.revision, meta.Branch), path, o.limit)
	if err != nil {
		return err
	}
	for _, c := range commits {
		fmt.Fprintf(out, "%s\t%s\t%s\n", shortOID(c.ID), c.AuthorName, c.Subject)
	}
	return nil
}

func cmdCat(ctx context.Context, out io.Writer, o *options, args []string) error {
	if len(args) != 1 {
		return usagef("cat requires REV:PATH")
	}
	rev, rawPath, ok := strings.Cut(args[0], ":")
	if !ok || rev == "" {
		return usagef("cat requires REV:PATH")
	}
	path, err := workspace.ValidatePath(rawPath)
	if err != nil {
		return usagef("%v", err)
	}
	backend, _, err := remoteFor(ctx, o)
	if err != nil {
		return err
	}
	content, err := backend.ReadFile(ctx, rev, path)
	if err != nil {
		return err
	}
	_, err = out.Write(content)
	return err
}

// workspaceRoot returns the workspace selected by -C or found from the working directory.
func workspaceRoot(o *options) (string, error) {
	if o.dir != "" {
		return o.dir, nil
	}
	return workspace.Find(".")
}

// openWorkspace opens the local workspace and binds it to its server.
func openWorkspace(ctx context.Context, o *options) (*workspace.Workspace, error) {
	root, err := workspaceRoot(o)
	if err != nil {
		return nil, err
	}
	meta, err := workspace.ReadMeta(root)
	if err != nil {
		return nil, err
	}
	backend, err := backendFactory(ctx, target{
		Address: pick(o.address, meta.Address),
		Token:   o.token,
		Storage: pick(o.storage, meta.Storage),
		Repo:    meta.Repo,
		User:    o.user,
	})
	if err != nil {
		return nil, err
	}
	return workspace.Open(root, backend)
}

// remoteFor binds a backend to the repository of the workspace without
// materializing anything. Read-only commands use it.
func remoteFor(ctx context.Context, o *options) (workspace.Backend, workspace.Meta, error) {
	root, err := workspaceRoot(o)
	if err != nil {
		return nil, workspace.Meta{}, err
	}
	meta, err := workspace.ReadMeta(root)
	if err != nil {
		return nil, workspace.Meta{}, err
	}
	backend, err := backendFactory(ctx, target{
		Address: pick(o.address, meta.Address),
		Token:   o.token,
		Storage: pick(o.storage, meta.Storage),
		Repo:    meta.Repo,
		User:    o.user,
	})
	if err != nil {
		return nil, workspace.Meta{}, err
	}
	return backend, meta, nil
}

// printChangeset prints c as one line. A batched land is followed by its members,
// one line each: "batch of N", then "actor original→landed" for every change.
// The text fields come from clients and file names, so they are escaped with
// displayText: a member could otherwise rewrite the lines of the others on the
// terminal.
func printChangeset(out io.Writer, c workspace.Changeset) {
	rewritten := ""
	if c.Rewritten {
		rewritten = " rewritten"
	}
	fmt.Fprintf(out, "%d\t%s\t%s..%s\t%s\t%s\t%s%s\n",
		c.Sequence, displayText(c.Reference), displayText(shortOID(c.OldOID)), displayText(shortOID(c.NewOID)),
		displayText(c.Operation), displayText(c.Actor), c.RecordedAt.Format(time.RFC3339), rewritten)

	if len(c.Members) == 0 {
		return
	}

	fmt.Fprintf(out, "  batch of %d\n", len(c.Members))
	for _, m := range c.Members {
		fmt.Fprintf(out, "    %s %s→%s%s\n", displayText(m.Actor),
			displayText(shortOID(m.OriginalCommit)), displayText(shortOID(m.Commit)), memberNote(m))
	}
}

// memberNote describes how a member of a batch was landed, for printChangeset.
// The resolved paths are escaped with displayText.
func memberNote(m workspace.ChangesetMember) string {
	var note strings.Builder
	if m.Rewritten {
		note.WriteString(" rewritten")
	}
	if len(m.ResolvedPaths) > 0 {
		note.WriteString(" resolved: " + displayList(m.ResolvedPaths))
	}

	return note.String()
}

// changesetJSON is the --json form of a changeset, one object per line. It is
// the event format of graft-superfan (superfan.Event). Object IDs are full.
// Members have the form of the changeset log, see member.Member.
type changesetJSON struct {
	Sequence   uint64          `json:"sequence"`
	Reference  string          `json:"reference"`
	OldOID     string          `json:"old_oid"`
	NewOID     string          `json:"new_oid"`
	Operation  string          `json:"operation"`
	Actor      string          `json:"actor"`
	RecordedAt time.Time       `json:"recorded_at"`
	Rewritten  bool            `json:"rewritten"`
	Members    []member.Member `json:"members,omitempty"`
}

// logMembers converts the members of a changeset to the form of the changeset
// log, which --json writes. It returns nil when there are none.
func logMembers(members []workspace.ChangesetMember) []member.Member {
	if len(members) == 0 {
		return nil
	}

	out := make([]member.Member, len(members))
	for i, m := range members {
		out[i] = member.Member{
			OriginalCommit: m.OriginalCommit,
			Commit:         m.Commit,
			Actor:          m.Actor,
			Rewritten:      m.Rewritten,
			ResolvedPaths:  m.ResolvedPaths,
		}
	}

	return out
}

// emitChangeset prints c in the output format selected by --json.
func emitChangeset(out io.Writer, o *options, c workspace.Changeset) error {
	if !o.jsonOut {
		printChangeset(out, c)
		return nil
	}
	line, err := json.Marshal(changesetJSON{
		Sequence:   c.Sequence,
		Reference:  c.Reference,
		OldOID:     c.OldOID,
		NewOID:     c.NewOID,
		Operation:  c.Operation,
		Actor:      c.Actor,
		RecordedAt: c.RecordedAt.UTC(),
		Rewritten:  c.Rewritten,
		Members:    logMembers(c.Members),
	})
	if err != nil {
		return err
	}
	_, err = out.Write(append(line, '\n'))
	return err
}

// parseAuthor parses "Name <email>". An empty value selects the default author.
func parseAuthor(s string) (workspace.Signature, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return workspace.Signature{Name: defaultAuthorName, Email: defaultAuthorEmail}, nil
	}
	open := strings.LastIndex(s, "<")
	if open < 0 {
		return workspace.Signature{Name: s, Email: defaultAuthorEmail}, nil
	}
	closing := strings.LastIndex(s, ">")
	if closing < open {
		return workspace.Signature{}, usagef(`author must look like "Name <email>"`)
	}
	name := strings.TrimSpace(s[:open])
	if name == "" {
		return workspace.Signature{}, usagef(`author must look like "Name <email>"`)
	}
	return workspace.Signature{Name: name, Email: strings.TrimSpace(s[open+1 : closing])}, nil
}

// checkStrategy rejects unknown land strategies. An empty strategy selects rebase.
func checkStrategy(s string) error {
	switch s {
	case "", workspace.StrategyRebase, workspace.StrategyMerge, workspace.StrategyFFOnly:
		return nil
	default:
		return usagef("unknown strategy %q (want rebase, merge or ff_only)", s)
	}
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func pick(first, fallback string) string {
	if first != "" {
		return first
	}
	return fallback
}

func shortOID(oid string) string {
	if len(oid) > shortOIDLength {
		return oid[:shortOIDLength]
	}
	return oid
}
