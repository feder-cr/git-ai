package conformance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/localrefs"
	"github.com/feder-cr/git-ai/internal/testhelper"
)

// secondPushBound is what a push may take after a failed one. An admission that git failed without
// releasing ends only at its hold bound of 10 s (coretest.EnableLocalRefs), and the failed push used
// up well under a second of it; an unloaded push takes well under a second. It is measured with the
// monotonic clock, because the wall clock of a WSL machine jumps by seconds.
const secondPushBound = 7 * time.Second

// TestConformanceFailedPushReleasesTheAdmission pushes so that git fails after the Preparing phase of
// its reference transaction, then measures the next push. Authoritative mode admits the writers of a
// repository one at a time: a transaction that keeps its admission blocks the next writer until the
// hold bound.
func TestConformanceFailedPushReleasesTheAdmission(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc string
		args []string
	}{
		{desc: "an atomic push with a directory/file conflict", args: []string{"--atomic", "main:refs/heads/side/below"}},
		{desc: "a push with a directory/file conflict", args: []string{"main:refs/heads/side/below"}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			backend := newGitaiBackend(t, gitaiMode{
				name: "authoritative-sqlite", standalone: true, coreMode: corestore.ModeAuthoritative, dialect: "sqlite",
			}, agentGraft())

			s := &session{
				t: t, ctx: testhelper.Context(t), remote: backend.newRemote(t), protocol: 0,
				root: testhelper.TempDir(t), observed: observations{},
			}
			seed := s.seeded()

			require.NotEqual(t, 0, s.push(seed, tc.args...), s.log.String())

			// Nobody holds the admission of the repository now.
			free, cancel := context.WithTimeout(s.ctx, secondPushBound)
			defer cancel()
			release := backend.core.RefAdmission.AcquireUnique(free, s.remote.path, localrefs.WriterHook)
			require.NotNil(t, release, "the failed push kept the admission of the repository")
			release()

			start := time.Now()
			code := s.push(seed, "main:refs/heads/after")
			elapsed := time.Since(start)
			require.Equal(t, 0, code, s.log.String())
			require.Less(t, elapsed, secondPushBound, "the push after %s waited: %s", tc.desc, strings.TrimSpace(s.log.String()))
			t.Logf("ADMISSION %s: the next push took %s", tc.desc, elapsed)
		})
	}
}
