package changelog

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/feder-cr/git-ai/internal/graft/changelog/member"
)

// shortOIDLength is the number of characters of an object ID that FormatMembers
// shows.
const shortOIDLength = 8

// Member is one change that a batched land folded into a changeset, see
// member.Member. The type and its JSON form live in a package of their own so
// that the graft CLI prints members without linking the log.
type Member = member.Member

// ValidateMembers checks that every member names both its original and its
// landed commit. It returns the first problem found, with the member's index.
func ValidateMembers(members []Member) error {
	for i, m := range members {
		if err := validateMember(m); err != nil {
			return fmt.Errorf("member %d: %w", i, err)
		}
	}

	return nil
}

// validateMember checks one member, see ValidateMembers.
func validateMember(m Member) error {
	switch {
	case m.OriginalCommit == "":
		return errors.New("original commit is empty")
	case m.Commit == "":
		return errors.New("commit is empty")
	case m.Rewritten && m.OriginalCommit == m.Commit:
		return errors.New("marked rewritten but commit equals original commit")
	}

	return nil
}

// validMembers returns the members that pass validateMember, in their order, and
// whether any member was dropped. Append uses it instead of failing: the
// reference update it records has already happened, so its entry must still be
// recorded, and the dropped members are flagged with MembersTruncated.
func validMembers(members []Member) ([]Member, bool) {
	if len(members) == 0 {
		return nil, false
	}

	valid := make([]Member, 0, len(members))
	for _, m := range members {
		if validateMember(m) == nil {
			valid = append(valid, m)
		}
	}

	return valid, len(valid) != len(members)
}

// normalizeMembers returns a copy of members in which the actors are valid UTF-8,
// as Entry actors must be. Resolved paths are copied as they are, because they
// are encoded exactly by MarshalJSON. It returns nil for an empty input so that
// an entry without members encodes without the field.
func normalizeMembers(members []Member) []Member {
	if len(members) == 0 {
		return nil
	}

	out := make([]Member, len(members))
	for i, m := range members {
		out[i] = Member{
			OriginalCommit: m.OriginalCommit,
			Commit:         m.Commit,
			Actor:          toValidUTF8(m.Actor),
			Rewritten:      m.Rewritten,
			ResolvedPaths:  append([]string(nil), m.ResolvedPaths...),
		}
	}

	return out
}

// toValidUTF8 replaces invalid bytes with U+FFFD, like Append does for actors.
func toValidUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, string(utf8.RuneError))
}

// FormatMembers renders members as one line each, for the SDK and the CLI. A
// line reads "<original> -> <landed> by <actor>", followed by " rewritten" and
// " resolved: <paths>" when those apply. Object IDs are shortened.
func FormatMembers(members []Member) []string {
	lines := make([]string, 0, len(members))
	for _, m := range members {
		var b strings.Builder
		fmt.Fprintf(&b, "%s -> %s", shortOID(m.OriginalCommit), shortOID(m.Commit))
		if m.Actor != "" {
			fmt.Fprintf(&b, " by %s", m.Actor)
		}
		if m.Rewritten {
			b.WriteString(" rewritten")
		}
		if m.Resolved() {
			fmt.Fprintf(&b, " resolved: %s", strings.Join(m.ResolvedPaths, ", "))
		}
		lines = append(lines, b.String())
	}

	return lines
}

// shortOID returns the first shortOIDLength characters of oid, or oid itself
// when it is shorter.
func shortOID(oid string) string {
	if len(oid) <= shortOIDLength {
		return oid
	}
	return oid[:shortOIDLength]
}
