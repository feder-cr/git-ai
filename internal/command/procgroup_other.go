//go:build !linux

package command

// procGroup has no way to identify a process group without proc(5), so a group never gets SIGKILL
// here: its number may be another's.
type procGroup struct{}

func newProcGroup(int) procGroup { return procGroup{} }

type procGroupProof struct{}

func (procGroup) proof() procGroupProof { return procGroupProof{} }

func (procGroupProof) stillOurs() bool { return false }
