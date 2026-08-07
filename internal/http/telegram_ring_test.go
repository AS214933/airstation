package http

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// TestTelegramSilencePacingStaysRealTime reproduces the two failure modes of
// the silence padding. With a consumer attached the producer must (1) emit at
// most one chunk per chunk duration, so the ring never accumulates a silence
// backlog that would delay the next track by many seconds, and (2) never
// starve the consumer while it does so. A regression that fills the ring with
// silence (or blocks inside the ring) fails one of the two assertions.
func TestTelegramSilencePacingStaysRealTime(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Server{
		telegramRing: newTelegramRing(telegramRingCapacity),
		logger:       log,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A real consumer attached, reading at real-time cadence: one silence
	// chunk per chunk duration.
	readerID, done := s.telegramRing.subscribe()
	defer done()

	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		for {
			if err := s.appendTelegramSilence(ctx); err != nil {
				return
			}
		}
	}()

	silence, err := s.telegramSilence()
	if err != nil {
		t.Fatalf("generate silence: %v", err)
	}
	maxBuffered := int64(0)
	longestWait := time.Duration(0)
	start := time.Now()
	for time.Since(start) < 2*time.Second {
		waitStart := time.Now()
		readCtx, readCancel := context.WithTimeout(ctx, time.Second)
		if _, err := s.telegramRing.readFrom(readCtx, readerID, len(silence)); err != nil {
			readCancel()
			t.Fatalf("read: %v", err)
		}
		readCancel()
		if w := time.Since(waitStart); w > longestWait {
			longestWait = w
		}
		if b := s.telegramRing.buffered(); b > maxBuffered {
			maxBuffered = b
		}
		time.Sleep(silenceChunkDuration)
	}
	cancel()
	<-producerDone

	t.Logf("longest consumer wait = %v, max ring backlog = %d bytes", longestWait, maxBuffered)
	if longestWait > 300*time.Millisecond {
		t.Fatalf("consumer waited %v for silence; padding is not paced for a real-time reader", longestWait)
	}
	// The ring must hold only the in-flight chunk, not a multi-second backlog
	// of silence (which would delay the next track by that many seconds).
	if maxBuffered > 5*int64(len(silence)) {
		t.Fatalf("silence backlog grew to %d bytes; silence must be paced at real time", maxBuffered)
	}
}
