package cli

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
)

func TestFinishRun(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)

	failure := errors.New("listener start failed for forward 2")
	var exit *exitError
	if err := finishRun(context.Background(), failure); !errors.As(err, &exit) || exit.code != exitFailure {
		t.Fatalf("got %v, want exit 1", err)
	}
	if !strings.Contains(output.String(), failure.Error()) {
		t.Fatalf("error not logged: %q", output.String())
	}

	output.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := finishRun(ctx, failure); err != nil || output.Len() != 0 {
		t.Fatalf("interrupted run returned %v and logged %q", err, output.String())
	}
	if err := finishRun(context.Background(), nil); err != nil {
		t.Fatalf("clean return gave %v", err)
	}
}
