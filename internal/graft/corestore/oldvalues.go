package corestore

import "context"

// oldValuesCheckedKey marks the context of a reference transaction whose old values git has
// checked against the local references under its lock before the Prepared phase.
type oldValuesCheckedKey struct{}

// WithOldValuesCheckedByGit returns ctx marked for the Prepared phase of a reference transaction
// that git prepared with an explicit old value for every update (the zero object ID meaning the
// reference must not exist), and whose Prepared phase runs only after that prepare succeeded:
// UpdaterWithHooks, which drives the reference-transaction hook in the server's own process. The
// voter then takes the expected old values from the hook input and does not read the local
// references (doc/graft/design/dbfirst-writes.md, stage 1a). The mark lives in the context, so
// a hook call that reaches the server over the HookService (a git process, a push) never has it.
func WithOldValuesCheckedByGit(ctx context.Context) context.Context {
	return context.WithValue(ctx, oldValuesCheckedKey{}, true)
}

// OldValuesCheckedByGit reports whether ctx carries the mark of WithOldValuesCheckedByGit.
func OldValuesCheckedByGit(ctx context.Context) bool {
	checked, _ := ctx.Value(oldValuesCheckedKey{}).(bool)
	return checked
}
