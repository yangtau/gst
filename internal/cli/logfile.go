package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

const maxLogSize = 1 << 20

// logFile is a background tunnel's log. Before a write grows the file past
// maxLogSize, it moves the file to path.1, replacing the older one, and
// starts a new file, so a tunnel keeps at most two. The descriptors in
// redirect follow the current file, so that what the Go runtime writes to
// standard error, such as a panic, lands there too.
type logFile struct {
	path     string
	redirect []int
	file     *os.File
	size     int64
}

func openLog(path string, redirect []int) (*logFile, error) {
	l := &logFile{path: path, redirect: redirect}
	return l, l.open()
}

func (l *logFile) open() error {
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
func (l *logFile) Write(p []byte) (int, error) {
	if l.size > 0 && l.size+int64(len(p)) > maxLogSize {
		// On failure, keep writing to the current file rather than lose lines.
		if err := os.Rename(l.path, l.path+".1"); err == nil {
			l.open()
		}
	}
	n, err := l.file.Write(p)
	l.size += int64(n)
	return n, err
}

func (l *logFile) Close() error { return l.file.Close() }
