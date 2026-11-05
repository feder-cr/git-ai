package e2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
)

// pausesBeforeSending makes the next push of c stop after c read the references of
// the server and before it sends its commands, until the returned resume is called.
// wait returns once c stopped there.
func (c staleCreateClient) pausesBeforeSending() (wait func(), resume func()) {
	c.t.Helper()

	dir := c.t.TempDir()
	ready, proceed := filepath.Join(dir, "ready"), filepath.Join(dir, "go")

	hooks := filepath.Join(c.path, ".git", "hooks")
	require.NoError(c.t, os.MkdirAll(hooks, 0o755))
	script := fmt.Sprintf("#!/bin/sh\n: >%s\nwhile [ ! -e %s ]; do sleep 0.05; done\n", ready, proceed)
	require.NoError(c.t, os.WriteFile(filepath.Join(hooks, "pre-push"), []byte(script), 0o755))

	wait = func() {
		c.t.Helper()
		require.Eventually(c.t, func() bool {
			_, err := os.Stat(ready)
			return err == nil || !errors.Is(err, os.ErrNotExist)
		}, time.Minute, 20*time.Millisecond, "the push reached its pre-push hook")
	}
	resume = func() {
		c.t.Helper()
		require.NoError(c.t, os.WriteFile(proceed, nil, 0o644))
	}
	return wait, resume
}

// TestStorageRejectedStaleUpdateIsNotRecorded reproduces the trace of
// doc/graft/design/dbfirst_bug_multinode.cfg with stock git: a non-atomic push whose
// old value of a branch is the database's value while the serving node's copy holds
// another one. git rejects that update against the copy and still feeds it to the
// reference-transaction hook, without marking it. The database must not record it:
// the client is told it was rejected, and the node's watermark would move over a
// change its copy does not have.
//
// The copy gets behind the database by an ABA of the branch on another node, between
// the push's reference advertisement and its commands: B moves the branch from v0 to
// v1, A catches up to v1, B moves it back to v0. The client then sends v0 -> v2, the
// value A advertised, which is the database's value again.
func TestStorageRejectedStaleUpdateIsNotRecorded(t *testing.T) {
	t.Parallel()

	const topic, side = "refs/heads/topic", "refs/heads/side"

	forEachStorageDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		who := workerName(0)
		graft, tokens := workerGraft(1)
		graft.AgentRPCs = []string{"/gitaly.SSHService/SSHReceivePack"}
		cluster := newStorageCluster(t, dialect, corestore.ModeAuthoritative, graft)
		a := cluster.addNode(t)
		rel := a.createRepository(t).GetRelativePath()

		seed := newPushClient(t, a, rel, "seed", who, tokens[who])
		v0 := seed.commit("base.txt")
		out, ok := seed.push("HEAD:refs/heads/main", "HEAD:"+topic)
		require.True(t, ok, out)

		b := cluster.addNode(t)
		tip, err := b.agent(t, rel, tokens[who]).ResolveRevision(b.ctx, "topic")
		require.NoError(t, err)
		require.Equal(t, v0, tip)

		other := newPushClient(t, b, rel, "other", who, tokens[who])
		v1 := other.commit("other.txt")

		pusher := newPushClient(t, a, rel, "pusher", who, tokens[who])
		v2 := pusher.commit("pusher.txt")
		wait, resume := pusher.pausesBeforeSending()

		type result struct {
			out string
			ok  bool
		}
		done := make(chan result, 1)
		go func() {
			out, ok := pusher.push("HEAD:"+topic, "HEAD:"+side)
			done <- result{out, ok}
		}()
		wait()

		// The ABA on node B, with node A caught up in the middle.
		out, ok = other.push("HEAD:" + topic)
		require.True(t, ok, out)
		tip, err = a.agent(t, rel, tokens[who]).ResolveRevision(a.ctx, "topic")
		require.NoError(t, err)
		require.Equal(t, v1, tip, "node A caught up to v1")
		out, ok = other.push("+" + v0 + ":" + topic)
		require.True(t, ok, out)
		require.Equal(t, corestore.Target(v0), a.dbRefs(t, rel)[topic])
		require.Equal(t, corestore.Target(v1), a.localRefs(t, rel)[topic], "node A's copy is behind the database")

		resume()
		res := <-done
		t.Logf("push of %s (v0 %s -> v2 %s) and %s, through node A:\n%s", topic, v0, v2, side, res.out)

		dbRefs := a.dbRefs(t, rel)
		require.Equal(t, corestore.Target(v0), dbRefs[topic],
			"git rejected the update of %s, so the database must not record it (v2 is %s)", topic, v2)
		require.False(t, res.ok, "the push with a rejected update is refused as a whole: %s", res.out)
		require.NotContains(t, dbRefs, side, "no reference of the refused push lands")

		a.requireConsistent(t, rel)
		b.requireConsistent(t, rel)
	})
}
