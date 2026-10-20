package cli

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestFinishRun(t *testing.T) {
	failure := errors.New("SSH connection failed")
	for _, tt := range []struct {
		name       string
		err        error
		lifetime   time.Duration
		elapsed    time.Duration
		cancel     bool
		cancelAt   time.Duration
		wantWait   time.Duration
		wantFailed bool
		wantLog    bool
	}{
		{name: "no minimum lifetime", err: failure, wantFailed: true, wantLog: true},
		{name: "early failure", err: failure, lifetime: time.Second, wantWait: time.Second, wantFailed: true, wantLog: true},
		{name: "only remaining lifetime", err: failure, lifetime: time.Second, elapsed: 400 * time.Millisecond, wantWait: 600 * time.Millisecond, wantFailed: true, wantLog: true},
		{name: "lifetime already elapsed", err: failure, lifetime: time.Second, elapsed: 2 * time.Second, wantFailed: true, wantLog: true},
		{name: "cancel during wait", err: failure, lifetime: time.Second, cancel: true, cancelAt: 100 * time.Millisecond, wantWait: 100 * time.Millisecond, wantLog: true},
		{name: "already cancelled", err: failure, lifetime: time.Second, cancel: true},
		{name: "clean return", lifetime: time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var output bytes.Buffer
				previous := log.Writer()
				log.SetOutput(&output)
				defer log.SetOutput(previous)

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var deadline time.Time
				if tt.lifetime > 0 {
					deadline = time.Now().Add(tt.lifetime)
				}
				time.Sleep(tt.elapsed)
				if tt.cancel {
					if tt.cancelAt == 0 {
						cancel()
					} else {
						go func() {
							time.Sleep(tt.cancelAt)
							cancel()
						}()
					}
				}
				start := time.Now()
				err := finishRun(ctx, tt.err, deadline)
				if tt.wantFailed {
					var exit *exitError
					if !errors.As(err, &exit) || exit.code != exitFailure {
						t.Fatalf("got %v, want exit 1", err)
					}
				} else if err != nil {
					t.Fatalf("got %v, want clean exit", err)
				}
				if waited := time.Since(start); waited != tt.wantWait {
					t.Errorf("waited %s, want %s", waited, tt.wantWait)
				}
				if logged := strings.Contains(output.String(), failure.Error()); logged != tt.wantLog {
					t.Errorf("log = %q, want error logged: %v", output.String(), tt.wantLog)
				}
			})
		})
	}
}

func TestFinishRunLogsBeforeWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var output bytes.Buffer
		previous := log.Writer()
		log.SetOutput(&output)
		defer log.SetOutput(previous)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- finishRun(ctx, errors.New("listener start failed for forward 2"), time.Now().Add(time.Second))
		}()
		synctest.Wait()
		if !strings.Contains(output.String(), "listener start failed for forward 2") {
			t.Fatalf("original error not logged before wait: %q", output.String())
		}
		select {
		case err := <-done:
			t.Fatalf("returned before deadline: %v", err)
		default:
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("cancellation returned %v, want clean exit", err)
		}
	})
}
