package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"google.golang.org/grpc"

	gitalyauth "github.com/feder-cr/git-ai/auth"
	"github.com/feder-cr/git-ai/client"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/graft/workspace"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// emptyTreeFor returns the empty tree of the object format of commitID, the left
// side of the diff of a root commit. The format follows from the length of the
// commit ID: a SHA-256 ID has as many hex digits as the SHA-256 zero ID.
func emptyTreeFor(commitID string) string {
	if len(commitID) == len(git.ObjectHashSHA256.ZeroOID) {
		return string(git.ObjectHashSHA256.EmptyTreeOID)
	}
	return string(git.ObjectHashSHA1.EmptyTreeOID)
}

// commitDetailer is implemented by backends that describe a commit. It is kept
// out of workspace.Backend: only show needs it.
type commitDetailer interface {
	// CommitDetails describes the commit that rev resolves to.
	CommitDetails(ctx context.Context, rev string) (commitDetails, error)
}

// commitDetails is the output of `graft show --json REV`, one JSON object. It is
// the commit format of graft-superfan (superfan.Details, see doc/graft/SUPERFAN.md).
// Every list and map is present, possibly empty. Files carry the changes against
// the first parent (against the empty tree for a root commit).
type commitDetails struct {
	OID string `json:"oid"`
	// PeeledFrom is set when REV is the full ID of another object that was
	// peeled to the commit: an annotated tag. A tag push records the tag's ID as
	// new_oid, so graft-superfan accepts details whose oid or peeled_from is
	// the new_oid of the event.
	PeeledFrom string              `json:"peeled_from,omitempty"`
	Parents    []string            `json:"parents"`
	Author     string              `json:"author"`
	Subject    string              `json:"subject"`
	Message    string              `json:"message"`
	Trailers   map[string][]string `json:"trailers"`
	Files      []fileDetails       `json:"files"`
}

// normalize makes every list and map present, so that the JSON form has [] and
// {} rather than null. Consumers such as graft-superfan expect that.
func (d *commitDetails) normalize() {
	if d.Parents == nil {
		d.Parents = []string{}
	}
	if d.Trailers == nil {
		d.Trailers = map[string][]string{}
	}
	if d.Files == nil {
		d.Files = []fileDetails{}
	}
}

// fileDetails is one file changed by a commit.
//
// A rename is reported as two entries, so that a rule or component that matches
// either side sees it: the new path with status "renamed" and the old path with
// status "renamed-from" (no line counts). Other changes have no status.
type fileDetails struct {
	Path      string `json:"path"`
	Status    string `json:"status,omitempty"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// bindShow sets the flags of show.
func bindShow(fs *flag.FlagSet, o *options) {
	fs.BoolVar(&o.jsonOut, "json", false, "print the commit as one JSON object")
}

// cmdShow describes one commit: its parents, author, message, trailers and the
// files it changed.
func cmdShow(ctx context.Context, out io.Writer, o *options, args []string) error {
	if len(args) != 1 {
		return usagef("show requires REV")
	}
	backend, _, err := remoteFor(ctx, o)
	if err != nil {
		return err
	}
	detailer, ok := backend.(commitDetailer)
	if !ok {
		return errors.New("show: the backend cannot describe commits")
	}
	d, err := detailer.CommitDetails(ctx, args[0])
	if err != nil {
		return err
	}
	d.normalize()

	if o.jsonOut {
		line, err := json.Marshal(d)
		if err != nil {
			return err
		}
		_, err = out.Write(append(line, '\n'))
		return err
	}
	// The author, the subject and the file paths come from other clients, so
	// they go through displayText, like the text of every other command.
	fmt.Fprintf(out, "commit %s\nAuthor: %s\n\n%s\n", d.OID, displayText(d.Author), displayText(d.Subject))
	for _, f := range d.Files {
		fmt.Fprintf(out, "+%d -%d\t%s\n", f.Additions, f.Deletions, displayText(f.Path))
	}
	return nil
}

// CommitDetails implements commitDetailer. It reads the commit with FindCommit,
// with its trailers, and its changes with DiffStats against the first parent. It
// dials a connection of its own, as the sdk does not expose one for these
// services.
func (b *sdkBackend) CommitDetails(ctx context.Context, rev string) (commitDetails, error) {
	conn, err := b.dialServices(ctx)
	if err != nil {
		return commitDetails{}, err
	}
	defer func() { _ = conn.Close() }()

	res, err := gitalypb.NewCommitServiceClient(conn).FindCommit(ctx, &gitalypb.FindCommitRequest{
		Repository: b.repository(),
		Revision:   []byte(rev),
		Trailers:   true,
	})
	if err != nil {
		return commitDetails{}, mapError(err)
	}
	c := res.GetCommit()
	if c == nil {
		return commitDetails{}, fmt.Errorf("%w: %s", workspace.ErrNotFound, rev)
	}

	// GitCommit.Body holds the whole message, subject included.
	message := string(c.GetBody())
	if message == "" {
		message = string(c.GetSubject())
	}
	d := commitDetails{
		OID:      c.GetId(),
		Parents:  c.GetParentIds(),
		Author:   signatureText(c.GetAuthor()),
		Subject:  string(c.GetSubject()),
		Message:  message,
		Trailers: map[string][]string{},
		Files:    []fileDetails{},
	}
	if d.Parents == nil {
		d.Parents = []string{}
	}
	// FindCommit peels tags. When rev is the full ID of an object other than
	// the commit, it named a tag; say so, as the caller may compare IDs.
	if isFullObjectID(rev) && !strings.EqualFold(rev, d.OID) {
		d.PeeledFrom = strings.ToLower(rev)
	}
	for _, t := range c.GetTrailers() {
		key := string(t.GetKey())
		d.Trailers[key] = append(d.Trailers[key], string(t.GetValue()))
	}

	left := emptyTreeFor(d.OID)
	if len(d.Parents) > 0 {
		left = d.Parents[0]
	}
	stream, err := gitalypb.NewDiffServiceClient(conn).DiffStats(ctx, &gitalypb.DiffStatsRequest{
		Repository:    b.repository(),
		LeftCommitId:  left,
		RightCommitId: d.OID,
	})
	if err != nil {
		return commitDetails{}, mapError(err)
	}
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return commitDetails{}, mapError(err)
		}
		for _, s := range resp.GetStats() {
			// Rename detection reports only the new path in Path. The old path
			// must be listed too, or a commit that moves a protected file out of
			// its directory would look as if it touched no protected path.
			oldPath := string(s.GetOldPath())
			newPath := string(s.GetPath())
			if oldPath != "" && oldPath != newPath {
				d.Files = append(d.Files,
					fileDetails{Path: newPath, Status: "renamed", Additions: int(s.GetAdditions()), Deletions: int(s.GetDeletions())},
					fileDetails{Path: oldPath, Status: "renamed-from"},
				)
				continue
			}
			d.Files = append(d.Files, fileDetails{
				Path:      newPath,
				Additions: int(s.GetAdditions()),
				Deletions: int(s.GetDeletions()),
			})
		}
	}
	return d, nil
}

// dialServices connects to the server of the backend for the commit and diff
// services. The caller closes the connection.
func (b *sdkBackend) dialServices(ctx context.Context) (*grpc.ClientConn, error) {
	var grpcOpts []grpc.DialOption
	if b.token != "" {
		grpcOpts = append(grpcOpts, grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(b.token)))
	}
	conn, err := client.DialContext(ctx, b.address, client.WithGrpcOptions(grpcOpts))
	if err != nil {
		return nil, fmt.Errorf("dial commit service: %w", err)
	}
	return conn, nil
}

// isFullObjectID reports whether s is a full SHA-1 or SHA-256 object ID in hex.
func isFullObjectID(s string) bool {
	if len(s) != len(git.ObjectHashSHA1.ZeroOID) && len(s) != len(git.ObjectHashSHA256.ZeroOID) {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

// signatureText renders an author as "Name <email>".
func signatureText(a *gitalypb.CommitAuthor) string {
	name := strings.TrimSpace(string(a.GetName()))
	email := string(a.GetEmail())
	if email == "" {
		return name
	}
	return name + " <" + email + ">"
}
