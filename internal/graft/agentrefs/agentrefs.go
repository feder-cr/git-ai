// Package agentrefs is the one rule of what references agents may see: refs/graft/ is internal to
// the server (the proposals of a land, graft-verify's green references), so an agent neither lists
// nor names a reference under it (PLAN10 14). The rule is enforced on the gRPC requests and
// responses of agents (server/auth) and in the advertisement of git upload-pack and receive-pack
// for agents (gitcmd). The blobs of a proposal reach an agent by their object IDs, in its package.
package agentrefs

import (
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
)

// HiddenPrefix is the namespace that agents do not see.
const HiddenPrefix = "refs/graft/"

// ServerNamespaces are the namespaces the server keeps under HiddenPrefix: the proposals of a land
// (refs/graft/proposals/) and graft-verify's green references (refs/graft/green/). Git completes a
// short revision "graft/<namespace>/..." to "refs/graft/<namespace>/..." when that reference
// exists, so an agent may not use that short form either. A new namespace under refs/graft/ is
// listed here.
var ServerNamespaces = []string{"proposals", "green"}

// Hidden reports whether a full reference name is one agents do not see.
func Hidden(ref string) bool {
	return strings.HasPrefix(ref, HiddenPrefix)
}

// VisibleToAgents reports whether an agent can see the full reference name at all: it is not
// under HiddenPrefix, nor under a namespace that the server never advertises to any client
// (git.InternalRefPrefixes of type InternalReferenceTypeHidden). A message to an agent names no
// other reference.
func VisibleToAgents(ref string) bool {
	if Hidden(ref) {
		return false
	}
	for prefix, kind := range git.InternalRefPrefixes {
		if kind == git.InternalReferenceTypeHidden && strings.HasPrefix(ref, prefix) {
			return false
		}
	}
	return true
}

// RevisionHidden reports whether a revision, reference or pattern an agent sends names a reference
// under HiddenPrefix: a part of its revision (before a ":path", between "..", "...", "^", "~", "@{",
// "=" as in --glob=<pattern>, or a space, line break, tab or NUL, which git takes as the end of a
// revision or of a line) that starts with refs/graft/, or with graft/<namespace>/ for a namespace of the server
// (ServerNamespaces), which git completes to refs/graft/<namespace>/. Other short names under
// graft/ are branches (the workspace's temporary branches are graft/<agent>/...): git completes
// them under refs/heads/, since nothing else exists under refs/graft/. The path after the first
// ":" is a path, not a reference.
func RevisionHidden(rev string) bool {
	rev, _, _ = strings.Cut(rev, ":")
	for _, part := range strings.FieldsFunc(rev, func(r rune) bool {
		return r == '.' || r == '^' || r == '~' || r == '@' || r == '{' || r == '}' || r == ' ' || r == '=' ||
			r == '\n' || r == '\r' || r == '\t' || r == 0
	}) {
		part = strings.TrimPrefix(part, "!")
		if strings.HasPrefix(part, HiddenPrefix) {
			return true
		}
		for _, ns := range ServerNamespaces {
			if part == "graft/"+ns || strings.HasPrefix(part, "graft/"+ns+"/") {
				return true
			}
		}
	}
	return false
}
