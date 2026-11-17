package daemon

import (
	"bytes"
	"sync"
)

// logTailSize bounds the end of the log that a daemon keeps.
const logTailSize = 128 << 10

// logTail keeps the end of the log, up to logTailSize, in whole lines.
type logTail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *logTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*logTailSize { // trimmed now and then, not at each line
		t.buf = append([]byte(nil), t.end()...)
	}
	return len(p), nil
}

// String returns the end of the log, oldest line first.
func (t *logTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.end())
}

// end returns the last logTailSize bytes of the log, from the start of a
// line. t.mu is held.
func (t *logTail) end() []byte {
	if len(t.buf) <= logTailSize {
		return t.buf
	}
	end := t.buf[len(t.buf)-logTailSize:]
	if i := bytes.IndexByte(end, '\n'); i >= 0 {
		end = end[i+1:]
	}
	return end
}
