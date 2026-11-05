package conformance

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/localrefs"
	"github.com/feder-cr/git-ai/internal/testhelper"
)

// TestConformanceFailedPushReleasesTheAdmission pushes so that git fails after the Preparing phase of
// its reference transaction. Authoritative mode admits the writers of a repository one at a time: a
// transaction that keeps its admission blocks the next writer until the hold bound of 10 s
// (coretest.EnableLocalRefs). The test asks the admission itself, which holds no time window: the
// failed push has ended, so that nothing may hold it. How long the next push takes is only logged: on
// a loaded machine a healthy one was seen to take 4.7 s.
func TestConformanceFailedPushReleasesTheAdmission(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc string
		args []string
		// bug, when set, is the suspected bug that keeps the admission. The case then expects the
		// admission to be held, so that it fails when the bug is fixed and the entry can go.
		bug string
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

			// Nobody holds the admission of the repository now: it is free at once.
			free, cancel := context.WithTimeout(s.ctx, time.Second)
			defer cancel()
			release := backend.core.RefAdmission.AcquireUnique(free, s.remote.path, localrefs.WriterHook)
			if tc.bug != "" {
				require.Nil(t, release, "the admission is free: remove the bug of this case")
			} else {
				require.NotNil(t, release, "the failed push kept the admission of the repository")
				release()
			}

			start := time.Now()
			code := s.push(seed, "main:refs/heads/after")
			elapsed := time.Since(start)
			require.Equal(t, 0, code, s.log.String())
			t.Logf("ADMISSION %s: the next push took %s", tc.desc, elapsed)
		})
	}
}
