package sdk

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// MaxLandRequestBytes is the gRPC message limit of the server (4 MiB). A Land request with a
// Resolution must fit in it; Land refuses a larger one before sending it.
const MaxLandRequestBytes = 4 << 20

// LandConflict returns the conflict package of a Land call that failed on its conflicts: every
// conflicting file with its sides, its conflicting hunks and what the server did with it (see
// doc/graft/RESOLVE.md). It is also sent when the call did not ask for automatic resolution.
// Resolve the files and land again with LandOptions.Resolution (see ResolutionFor).
func LandConflict(err error) (*gitalypb.LandConflict, bool) {
	return detailOf[*gitalypb.LandConflict](err)
}

// ConflictSides are the contents of the sides of a conflicting file. An absent side is nil, and so
// is a side that cannot be read: Unreadable gives the reason of each, by side ("base", "ours",
// "theirs"), and "proposal" for a proposal whose content cannot be read.
type ConflictSides struct {
	Base, Ours, Theirs []byte
	Unreadable         map[string]string
	// Proposal is the resolver's resolution of the file, when it made one: a model's, which did not
	// land because no verification passed on it (OUTCOME_PROPOSED), or the rules' in a land that
	// failed for another file.
	// Land it, edited or not, as your own (ResolutionFromProposals).
	Proposal *Proposal
}

// Proposal is the resolver's resolution of one conflicting file.
type Proposal struct {
	// Delete proposes to delete the path; otherwise Content is written with Mode. Content is nil
	// when it cannot be read (ConflictSides.Unreadable["proposal"]).
	Delete  bool
	Mode    string
	Content []byte
	// Resolver names what made it; Explanation is the resolver's, cut to 512 bytes.
	Resolver, Explanation string
}

// ConflictSides reads the sides of every file of a conflict package, by path, with GetObjects. A
// side that cannot be read costs only that side: one the package marks unavailable (the merge
// wrote it for the conflict, and the repository did not keep it), or one the repository does not
// have, is nil with its reason in Unreadable. An error is a failure to read at all.
func (r *Repo) ConflictSides(ctx context.Context, c *gitalypb.LandConflict) (map[string]ConflictSides, error) {
	var oids []string
	seen := map[string]bool{}
	for _, f := range c.GetFiles() {
		for _, s := range []*gitalypb.LandConflictSide{f.GetBase(), f.GetOurs(), f.GetTheirs()} {
			if oid := s.GetOid(); oid != "" && !seen[oid] {
				seen[oid] = true
				oids = append(oids, oid)
			}
		}
		if oid := f.GetProposal().GetOid(); oid != "" && !seen[oid] {
			seen[oid] = true
			oids = append(oids, oid)
		}
	}
	data := make(map[string][]byte, len(oids))
	missing := map[string]bool{}
	if len(oids) > 0 {
		objects, err := r.GetObjects(ctx, oids)
		if err != nil {
			return nil, err
		}
		for _, o := range objects {
			if o.Missing {
				missing[o.OID] = true
				continue
			}
			data[o.OID] = o.Data
		}
	}
	out := make(map[string]ConflictSides, len(c.GetFiles()))
	for _, f := range c.GetFiles() {
		var sides [3][]byte
		unreadable := map[string]string{}
		for i, s := range []*gitalypb.LandConflictSide{f.GetBase(), f.GetOurs(), f.GetTheirs()} {
			name := []string{"base", "ours", "theirs"}[i]
			switch {
			case s == nil:
			case s.GetOid() == "":
				unreadable[name] = s.GetUnavailable()
			case missing[s.GetOid()]:
				unreadable[name] = "the blob " + s.GetOid() + " is missing from the repository"
			default:
				sides[i] = data[s.GetOid()]
				if sides[i] == nil {
					sides[i] = []byte{}
				}
			}
		}
		cs := ConflictSides{Base: sides[0], Ours: sides[1], Theirs: sides[2]}
		if prop := f.GetProposal(); prop != nil {
			cs.Proposal = &Proposal{Delete: prop.GetAction() == "delete", Resolver: prop.GetResolver(), Explanation: prop.GetExplanation()}
			if !cs.Proposal.Delete {
				cs.Proposal.Mode = prop.GetMode()
				switch {
				case prop.GetOid() == "" || missing[prop.GetOid()]:
					unreadable["proposal"] = "the blob " + prop.GetOid() + " of the proposal is missing from the repository"
				default:
					cs.Proposal.Content = data[prop.GetOid()]
					if cs.Proposal.Content == nil {
						cs.Proposal.Content = []byte{}
					}
				}
			}
		}
		if len(unreadable) > 0 {
			cs.Unreadable = unreadable
		}
		out[string(f.GetPath())] = cs
	}
	return out, nil
}

// Resolution is the caller's resolution of the conflicts of a conflict package, for
// LandOptions.Resolution. It must resolve every conflicting path exactly once.
type Resolution struct {
	// Tip and Commit are the tip and the landed commit of the package (LandConflict.Tip and
	// LandConflict.Commit).
	Tip, Commit string
	// Files resolve the conflicting paths.
	Files []ResolvedFile
}

// ResolvedFile resolves one conflicting path: Delete it, or write Content with Mode.
type ResolvedFile struct {
	Path    string
	Delete  bool
	Mode    string
	Content []byte
}

// ResolutionFor returns an empty resolution of the conflicts of c: its tip and commit, and no
// file yet.
func ResolutionFor(c *gitalypb.LandConflict) *Resolution {
	return &Resolution{Tip: c.GetTip(), Commit: c.GetCommit()}
}

// ResolutionFromProposals returns the resolution of the conflicts of c that takes every readable
// proposal of sides (ConflictSides) as it is. The paths without one are left out: add them, and
// review or edit the proposals, before landing it. The proposals land as yours.
func ResolutionFromProposals(c *gitalypb.LandConflict, sides map[string]ConflictSides) *Resolution {
	res := ResolutionFor(c)
	for _, f := range c.GetFiles() {
		p := string(f.GetPath())
		prop := sides[p].Proposal
		switch {
		case prop == nil:
		case prop.Delete:
			res.Files = append(res.Files, ResolvedFile{Path: p, Delete: true})
		case prop.Content != nil:
			res.Files = append(res.Files, ResolvedFile{Path: p, Mode: prop.Mode, Content: prop.Content})
		}
	}
	return res
}

// toProto returns the resolution as the request field.
func (r *Resolution) toProto() *gitalypb.LandResolution {
	out := &gitalypb.LandResolution{Tip: r.Tip, Commit: r.Commit}
	for _, f := range r.Files {
		rf := &gitalypb.LandResolvedFile{Path: []byte(f.Path), Action: gitalypb.LandResolvedFile_ACTION_WRITE, Mode: f.Mode, Content: f.Content}
		if f.Delete {
			rf.Action, rf.Mode, rf.Content = gitalypb.LandResolvedFile_ACTION_DELETE, "", nil
		}
		out.Files = append(out.Files, rf)
	}
	return out
}

// checkLandRequestSize refuses a request that the server's 4 MiB message limit would refuse,
// with a clear error instead of a transport failure.
func checkLandRequestSize(req *gitalypb.UserLandRequest) error {
	if n := proto.Size(req); n > MaxLandRequestBytes {
		return status.Errorf(codes.InvalidArgument,
			"the land request with its resolution is %d bytes, more than the server's 4 MiB message limit: resolve fewer or smaller files per land", n)
	}
	return nil
}
