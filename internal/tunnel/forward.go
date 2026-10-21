package tunnel

import (
	"context"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

// forward relays one connection accepted on r's listener to r.target, or
// to the target the client requests over SOCKS5.
func (t *tunnel) forward(ctx context.Context, r route, incoming net.Conn) {
	defer incoming.Close()
	defer closeOnDone(ctx, incoming)()

	target := r.target
	if r.socks {
		incoming.SetDeadline(time.Now().Add(t.opts.ConnectTimeout))
		var err error
		if target, err = socksRequest(incoming); err != nil {
			if ctx.Err() == nil {
				log.Printf("SOCKS request failed for forward %d", r.id)
			}
			return
		}
		incoming.SetDeadline(time.Time{})
	}

	dialCtx, cancel := context.WithTimeout(ctx, t.opts.ConnectTimeout)
	outgoing, err := r.dial(dialCtx, "tcp", target)
	cancel()
	if r.socks {
		socksReply(incoming, socksReplyCode(err))
	}
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("target connection failed for forward %d", r.id)
		}
		return
	}
	defer outgoing.Close()
	defer closeOnDone(ctx, outgoing)()

	relay(incoming, outgoing)
}

// relay copies data in both directions until both sides are done. A clean
// EOF is passed on as a half-close so the other direction keeps flowing;
// any error tears down both connections.
func relay(a, b io.ReadWriteCloser) {
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
