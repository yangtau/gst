package tunnel

import (
	"context"
	"errors"
	"time"

	"golang.org/x/crypto/ssh"
)

// keepalive probes the server every interval and returns an error once a
// probe fails or stays unanswered for longer than timeout. It returns nil
// when ctx is cancelled. The caller must close client afterwards to release
// a probe that is still waiting for its reply.
func keepalive(ctx context.Context, client *ssh.Client, interval, timeout time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		err := probe(ctx, client, timeout)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// probe sends one keepalive request and waits up to timeout for its reply.
//
// SendRequest has no timeout of its own, and x/crypto/ssh serializes global
// requests that want a reply, so a probe can also queue behind a stalled
// request such as tcpip-forward. A probe that gives up leaves the request
// pending; only closing the client releases it.
func probe(ctx context.Context, client *ssh.Client, timeout time.Duration) error {
	reply := make(chan error, 1)
	go func() {
		_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
		reply <- err
	}()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case err := <-reply:
		// Any reply, including a refusal, proves that the server is alive.
		if err != nil {
			return errors.New("SSH keepalive failed")
		}
		return nil
	case <-ctx.Done():
		return errors.New("SSH keepalive timed out")
	}
}
