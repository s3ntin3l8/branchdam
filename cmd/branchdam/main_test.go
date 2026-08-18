package main

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestWaitBounded(t *testing.T) {
	t.Run("completes before context deadline", func(t *testing.T) {
		buf := &bytes.Buffer{}
		log := slog.New(slog.NewTextHandler(buf, nil))
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()

		completed := false
		waitBounded(ctx, log, "quickComponent", func() {
			completed = true
		})

		if !completed {
			t.Errorf("expected waitBounded function to complete")
		}
		if bytes.Contains(buf.Bytes(), []byte("shutdown wait timed out")) {
			t.Errorf("unexpected timeout warning in logs")
		}
	})

	t.Run("times out and logs warning when wait function hangs", func(t *testing.T) {
		buf := &bytes.Buffer{}
		log := slog.New(slog.NewTextHandler(buf, nil))
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		slowDone := make(chan struct{})
		defer close(slowDone)

		start := time.Now()
		waitBounded(ctx, log, "slowComponent", func() {
			<-slowDone
		})
		elapsed := time.Since(start)

		if elapsed < 40*time.Millisecond || elapsed > 500*time.Millisecond {
			t.Errorf("waitBounded did not respect context deadline: took %v", elapsed)
		}
		if !bytes.Contains(buf.Bytes(), []byte("shutdown wait timed out")) {
			t.Errorf("expected timeout log warning, got: %s", buf.String())
		}
		if !bytes.Contains(buf.Bytes(), []byte("slowComponent")) {
			t.Errorf("expected component name in log output, got: %s", buf.String())
		}
	})
}
