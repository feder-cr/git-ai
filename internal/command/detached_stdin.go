package command

import (
	"fmt"
	"io"
	"os"
)

// DetachStdin returns a file to pass as the standard input of a command whose input comes from a
// client. Given any other io.Reader, exec.Cmd copies it in a goroutine, and Wait waits for that
// goroutine even after the process exited: as long as the client keeps its stream open, the process
// counts as running, and everything tied to its end stays held (WithExitHook).
//
// The returned file is the read end of a pipe that stdin is copied into by a goroutine of this
// function. The caller closes the file once the command has ended. copied, when not nil, runs after
// the copy ended. The goroutine ends when stdin ends or fails, or when a write fails after the file
// was closed, so it may outlive the command as long as stdin blocks: stdin must end by itself, as
// the stream of an RPC does when its handler returns.
func DetachStdin(stdin io.Reader, copied func()) (*os.File, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("creating pipe: %w", err)
	}

	go func() {
		_, _ = io.Copy(writer, stdin)
		_ = writer.Close()
		if copied != nil {
			copied()
		}
	}()

	return reader, nil
}
