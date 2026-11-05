package safe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
)

type lockingFileWriterState int

const (
	lockingFileWriterStateOpen = lockingFileWriterState(iota)
	lockingFileWriterStateLocked
	lockingFileWriterStateClosed
)

// ErrFileAlreadyLocked is returned when trying to lock a file which has already been locked by
// another concurrent process.
var ErrFileAlreadyLocked = errors.New("file already locked")

// ErrFileConcurrentlyCreated, ErrFileConcurrentlyDeleted and ErrFileConcurrentlyModified are
// returned by Lock and Commit when the target file was created, deleted or modified after the
// LockingFileWriter was created.
var (
	ErrFileConcurrentlyCreated  = errors.New("file concurrently created")
	ErrFileConcurrentlyDeleted  = errors.New("file concurrently deleted")
	ErrFileConcurrentlyModified = errors.New("file concurrently modified")
)

// LockingFileWriter is a FileWriter which locks the target file on commit and checks whether it
// has been modified since the LockingFileWriter has been created. The user must first create a new
// LockingFileWriter via `NewLockingFileWriter()`, at which point it is open for writes. The writer
// must be `Lock()`ed before `Commit()`ting changes.
type LockingFileWriter struct {
	writer *FileWriter
	fi     os.FileInfo
	state  lockingFileWriterState
}

// LockingFileWriterConfig contains configuration for the `NewLockingFileWriter()` function.
type LockingFileWriterConfig struct {
	// FileWriterConfig is the configuration for the embedded FileWriter.
	FileWriterConfig
	// SeedContents will seed the FileWriter's file with contents of the target file if
	// set. If the target file does not exist, then the file remains empty.
	SeedContents bool
}

// NewLockingFileWriter creates a new LockingFileWriter for the given path. At creation, it
// stats the target file and caches its current size and last modification time such that it can
// compare on commit whether the file has changed.
func NewLockingFileWriter(path string, optionalCfg ...LockingFileWriterConfig) (*LockingFileWriter, error) {
	var cfg LockingFileWriterConfig
	if len(optionalCfg) == 1 {
		cfg = optionalCfg[0]
	} else if len(optionalCfg) > 1 {
		return nil, fmt.Errorf("locking file writer created with more than one config")
	}

	targetFile, err := os.Open(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("opening target file: %w", err)
	}
	defer targetFile.Close()

	var targetFileInfo os.FileInfo
	if targetFile != nil {
		targetFileInfo, err = targetFile.Stat()
		if err != nil {
			return nil, fmt.Errorf("statting target file: %w", err)
		}
	}

	writer, err := NewFileWriter(path, cfg.FileWriterConfig)
	if err != nil {
		return nil, fmt.Errorf("creating file writer: %w", err)
	}

	if targetFile != nil && cfg.SeedContents {
		_, err := io.Copy(writer, targetFile)
		if err != nil {
			return nil, fmt.Errorf("seeding file writer: %w", err)
		}
	}

	return &LockingFileWriter{
		writer: writer,
		fi:     targetFileInfo,
	}, nil
}

// Write writes to the FileWriter. Must be called on an open LockingFileWriter.
func (fw *LockingFileWriter) Write(p []byte) (int, error) {
	if fw.state != lockingFileWriterStateOpen {
		return 0, fmt.Errorf("file writer not accepting writes")
	}

	return fw.writer.Write(p)
}

// Close closes the FileWriter and removes any locks and temporary files without updating the target
// file. Does nothing if the file has already been closed.
func (fw *LockingFileWriter) Close() error {
	var err error
	switch fw.state {
	case lockingFileWriterStateOpen:
		// No lock has been taken yet, so we don't have to unlock.
	case lockingFileWriterStateLocked:
		err = fw.unlock()
	case lockingFileWriterStateClosed:
		return nil
	default:
		return fmt.Errorf("invalid state %d", fw.state)
	}

	if writerErr := fw.writer.Close(); writerErr != nil && err == nil {
		err = fmt.Errorf("closing writer: %w", writerErr)
	}

	fw.state = lockingFileWriterStateClosed

	if err != nil {
		return err
	}

	return nil
}

// Lock locks the file writer such that no other process can concurrently update the same file. Must
// be called on an open LockingFileWriter.
func (fw *LockingFileWriter) Lock() error {
	if fw.state != lockingFileWriterStateOpen {
		return fmt.Errorf("file writer not lockable")
	}

	if err := fw.checkConcurrentModification(); err != nil {
		return err
	}

	lock, err := os.OpenFile(fw.lockPath(), os.O_CREATE|os.O_EXCL|os.O_RDONLY, mode.File)
	if err != nil {
		if os.IsExist(err) {
			return ErrFileAlreadyLocked
		}

		return fmt.Errorf("creating lock file: %w", err)
	}
	_ = lock.Close()

	fw.state = lockingFileWriterStateLocked

	return nil
}

func (fw *LockingFileWriter) unlock() error {
	// We only want to unlock in case we have locked this file ourselves. Otherwise, we risk
	// removing the lock from another, concurrent locking file writer.
	if fw.state != lockingFileWriterStateLocked {
		return fmt.Errorf("file writer not locked")
	}

	if err := os.Remove(fw.lockPath()); err != nil {
		return fmt.Errorf("removing lock file: %w", err)
	}

	fw.state = lockingFileWriterStateClosed

	return nil
}

// Commit writes whatever has been written to the Filewriter to the target file if and only if the
// target file has not been modified meanwhile. The writer must be `Lock()`ed first. The writer
// will be closed after this call, with all locks and temporary files having been removed.
func (fw *LockingFileWriter) Commit(ctx context.Context) (returnedErr error) {
	if fw.state != lockingFileWriterStateLocked {
		return fmt.Errorf("file writer not locked")
	}

	// While we have already checked that there was no concurrent modification when locking the
	// file, we do so again here in order to verify that no other processes which are unaware of
	// the locking semantics have changed the file. This may be overly cautious, but on the
	// other hand the single stat(3P) call shouldn't be all that expensive in the first place.
	if err := fw.checkConcurrentModification(); err != nil {
		return err
	}

	if err := fw.writer.Commit(ctx); err != nil {
		return fmt.Errorf("committing file: %w", err)
	}

	if err := fw.unlock(); err != nil {
		return fmt.Errorf("unlocking file: %w", err)
	}

	return nil
}

func (fw *LockingFileWriter) checkConcurrentModification() error {
	fi, err := os.Stat(fw.writer.path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("statting path: %w", err)
	}

	if fw.fi == nil && fi != nil {
		return ErrFileConcurrentlyCreated
	}
	if fw.fi != nil && fi == nil {
		return ErrFileConcurrentlyDeleted
	}
	if fw.fi != nil && fi != nil {
		if fw.fi.Size() != fi.Size() || fw.fi.ModTime() != fi.ModTime() || fw.fi.Mode() != fi.Mode() {
			return ErrFileConcurrentlyModified
		}
	}

	return nil
}

// Path returns the path of the intermediate file the FileWriter is writing to. Exposing the path
// allows an external process to write to the file directly. While it would be preferable to use the
// io.Writer interface instead, this is not easily doable e.g. for Git processes.
func (fw *LockingFileWriter) Path() string {
	return fw.writer.tmpFile.Name()
}

func (fw *LockingFileWriter) lockPath() string {
	return fw.writer.path + ".lock"
}
