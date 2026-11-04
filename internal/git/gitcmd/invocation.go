package gitcmd

import (
	"sync"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
)

// InvocationRequired reports whether the reference transactions run with cfg carry an invocation
// (ChangelogMeta.Invocation): the changeset log pairs their phases by it, and in authoritative mode
// the storage core ties their admission to the life of their process.
func InvocationRequired(cfg config.Cfg) bool {
	return !cfg.Graft.Changelog.Disabled || cfg.Graft.Storage.Mode == "authoritative"
}

// liveInvocations holds the invocations of this server process whose git process, or in-process
// driver of the hook, has not ended. The hook of a transaction reaches the server through its
// internal socket, so the server that runs the hook is the server that owns the process. Like the
// commit handles of the MVCC caches, it is per process and not per command factory: invocations are
// random, and the code that runs the hook has no access to the factory that started the process.
var liveInvocations = struct {
	sync.Mutex
	// exit maps a live invocation to the function to call when it ends, nil when none is set.
	exit map[string]func()
}{exit: map[string]func(){}}

// BeginInvocation declares that the process of invocation is live, and returns the function that
// declares its end. The owner of the process calls it once the process has exited, whatever git
// reported: git does not run the "aborted" phase of the reference-transaction hook for a transaction
// whose prepare fails in git update-ref, nor for a process that is killed. Only the end of the
// process then tells that nothing more comes. Calling end again does nothing. An empty invocation
// is no invocation.
func BeginInvocation(invocation string) (end func()) {
	if invocation == "" {
		return func() {}
	}

	liveInvocations.Lock()
	liveInvocations.exit[invocation] = nil
	liveInvocations.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			liveInvocations.Lock()
			exit := liveInvocations.exit[invocation]
			delete(liveInvocations.exit, invocation)
			liveInvocations.Unlock()

			if exit != nil {
				exit()
			}
		})
	}
}

// InvocationLive reports whether invocation has not ended. An end declared before this call has
// taken the invocation out already, whether or not its function has run; one that comes later still
// calls the function set with OnInvocationExit.
func InvocationLive(invocation string) bool {
	liveInvocations.Lock()
	defer liveInvocations.Unlock()

	_, live := liveInvocations.exit[invocation]
	return live
}

// OnInvocationExit sets the function to call when invocation ends, replacing the one set before. It
// reports false, and sets nothing, when the invocation is not live: its process has exited already
// (a hook that outlived a killed git process), or another process owns it. Whatever the interleaving
// with the end of the invocation, either this reports false or exit gets called. exit must not block.
func OnInvocationExit(invocation string, exit func()) bool {
	liveInvocations.Lock()
	defer liveInvocations.Unlock()

	if _, live := liveInvocations.exit[invocation]; !live {
		return false
	}
	liveInvocations.exit[invocation] = exit
	return true
}
