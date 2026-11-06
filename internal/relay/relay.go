// Package relay copies a connection's bytes to another connection.
package relay

import (
	"io"
	"sync"
)

// Pipe copies data in both directions until both sides are done, and
// closes both. A clean EOF is passed on as a half-close, so that the
// other direction keeps flowing; any error tears down both connections.
func Pipe(a, b io.ReadWriteCloser) {
	defer a.Close()
	defer b.Close()
	abort := func() {
		a.Close()
		b.Close()
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		if copyThenCloseWrite(a, b) != nil {
			abort()
		}
	})
	wg.Go(func() {
		if copyThenCloseWrite(b, a) != nil {
			abort()
		}
	})
	wg.Wait()
}

func copyThenCloseWrite(dst io.WriteCloser, src io.Reader) error {
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	if hc, ok := dst.(interface{ CloseWrite() error }); ok {
		return hc.CloseWrite()
	}
	return dst.Close()
}
