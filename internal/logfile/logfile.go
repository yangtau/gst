// Package logfile writes logs to files that rotate at MaxSize.
package logfile

import (
	"bytes"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// MaxSize is the size at which a log file rotates.
const MaxSize = 1 << 20

// File is a log file. Before a write grows the file past MaxSize, it moves
// the file to path.1, replacing the older one, and starts a new file, so
// a log keeps at most two files. The descriptors in
// redirect follow the current file, so that what the Go runtime writes to
// standard error, such as a panic, lands there too.
type File struct {
	path     string
	redirect []int
	file     *os.File
	size     int64
}

// Open opens or creates the log file at path, redirecting the file
// descriptors in redirect to it.
func Open(path string, redirect []int) (*File, error) {
	l := &File{path: path, redirect: redirect}
	return l, l.open()
}

func (l *File) open() error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	for _, fd := range l.redirect {
		if err := unix.Dup2(int(f.Fd()), fd); err != nil {
			f.Close()
			return err
		}
	}
	if l.file != nil {
		l.file.Close()
	}
	l.file, l.size = f, info.Size()
	return nil
}

// Write is called by one log.Logger, which serializes calls.
func (l *File) Write(p []byte) (int, error) {
	if l.size > 0 && l.size+int64(len(p)) > MaxSize {
		// On failure, keep writing to the current file rather than lose lines.
		if err := os.Rename(l.path, l.path+".1"); err == nil {
			l.open()
		}
	}
	n, err := l.file.Write(p)
	l.size += int64(n)
	return n, err
}

func (l *File) Close() error { return l.file.Close() }

// End returns the last size bytes of the log at path, across its latest
// rotation, from the start of a line: empty when there is no log.
func End(path string, size int) (string, error) {
	var text []byte
	// The file before the latest rotation comes first.
	for _, p := range []string{path + ".1", path} {
		data, err := os.ReadFile(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		text = append(text, data...)
	}
	if len(text) > size {
		text = text[len(text)-size:]
		if i := bytes.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	return string(text), nil
}
