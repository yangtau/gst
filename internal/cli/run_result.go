package cli

import (
	"context"
	"log"
)

// finishRun logs the error that ended a foreground tunnel and turns it into
// exit status 1. An interrupted tunnel exits cleanly.
func finishRun(ctx context.Context, err error) error {
	if err == nil || ctx.Err() != nil {
		return nil
	}
	log.Print(err)
	return &exitError{exitFailure}
}
