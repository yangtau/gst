package cli

import (
	"context"
	"log"
	"time"
)

func finishRun(ctx context.Context, err error, notBefore time.Time) error {
	if err == nil || ctx.Err() != nil {
		return nil
	}
	log.Print(err)
	if remaining := time.Until(notBefore); remaining > 0 {
		timer := time.NewTimer(remaining)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return &exitError{exitFailure}
}
