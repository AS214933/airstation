package http

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/cheatsnake/airstation/internal/pkg/ffmpeg"
)

// telegramRingCapacity is how much streamed audio the shared Telegram stream
// keeps buffered. At 128 kbps ≈ 16 KB/s, so 64 KiB ≈ 4 s of audio. The
// producer absorbs track-switch gaps with silence and chained playlists, so
// the ring only needs to bridge remux startup (~100-300 ms) and bound how far
// a reconnecting consumer jumps ahead to the live edge.
const telegramRingCapacity = 64 * 1024

// silenceChunkDuration is the granularity at which the producer emits silence
// while no track is ready, so at most that much silence is queued when the
// next track becomes available.
const silenceChunkDuration = 100 * time.Millisecond

// maxTelegramPlayedTracks bounds how many recently streamed playlist paths
// the producer remembers. Each prepared track gets a fresh on-disk path, so
// the bound only guards against unbounded growth, never against a legitimate
// track replay.
const maxTelegramPlayedTracks = 32

// telegramRing is a bounded, multi-reader byte buffer that carries the
// continuous ADTS stream from the shared producer to every HTTP connection.
//
// The producer appends and waits when the ring is full relative to the
// slowest reader, so no reader ever loses data. A reader subscribes at the
// live edge (the newest byte), which makes reconnects resume near the current
// stream position instead of restarting the current track from the start.
type telegramRing struct {
	mu       sync.Mutex
	capacity int
	buf      []byte

	total   int64           // total bytes appended
	readers map[int64]int64 // reader id -> next offset to read
	nextID  int64
	slowest int64 // smallest offset any active reader waits at
	notify  chan struct{}
}

func newTelegramRing(capacity int) *telegramRing {
	return &telegramRing{
		capacity: capacity,
		buf:      make([]byte, capacity),
		readers:  make(map[int64]int64),
		notify:   make(chan struct{}),
	}
}

// append adds p to the ring, blocking while the ring is full relative to the
// slowest reader.
func (r *telegramRing) append(p []byte) {
	for len(p) > 0 {
		r.mu.Lock()
		for r.total-r.slowest >= int64(r.capacity) {
			ch := r.notify
			r.mu.Unlock()
			select {
			case <-ch:
			case <-time.After(2 * time.Second):
			}
			r.mu.Lock()
		}
		pos := int(r.total % int64(r.capacity))
		free := r.capacity - int(r.total-r.slowest)
		chunk := min(len(p), free, r.capacity-pos)
		copy(r.buf[pos:pos+chunk], p[:chunk])
		r.total += int64(chunk)
		p = p[chunk:]
		r.wakeLocked()
		r.mu.Unlock()
	}
}

// Write implements io.Writer so the ring can be used as the stdout of the
// ADTS remux process. It always reports success: append only blocks.
func (r *telegramRing) Write(p []byte) (int, error) {
	r.append(p)
	return len(p), nil
}

// subscribe registers a reader at the live edge and returns its id together
// with a function that unregisters it.
func (r *telegramRing) subscribe() (int64, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.nextID
	r.nextID++
	r.readers[id] = r.total
	r.recomputeSlowestLocked()
	return id, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.readers, id)
		r.recomputeSlowestLocked()
		r.wakeLocked()
	}
}

// readFrom returns up to max bytes for the reader, blocking until data is
// available or ctx is done. The returned bytes have been consumed: the next
// call continues after them.
func (r *telegramRing) readFrom(ctx context.Context, id int64, max int) ([]byte, error) {
	r.mu.Lock()
	for r.total <= r.readers[id] {
		ch := r.notify
		r.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		r.mu.Lock()
	}
	offset := r.readers[id]
	avail := min(int(r.total-offset), max)
	pos := int(offset % int64(r.capacity))
	chunk := min(avail, r.capacity-pos)
	data := make([]byte, chunk)
	copy(data, r.buf[pos:pos+chunk])
	r.readers[id] = offset + int64(chunk)
	r.recomputeSlowestLocked()
	r.wakeLocked()
	r.mu.Unlock()
	return data, nil
}

func (r *telegramRing) recomputeSlowestLocked() {
	if len(r.readers) == 0 {
		r.slowest = r.total
		return
	}
	slowest := int64(0)
	first := true
	for _, off := range r.readers {
		if first || off < slowest {
			slowest = off
			first = false
		}
	}
	r.slowest = slowest
}

func (r *telegramRing) wakeLocked() {
	close(r.notify)
	r.notify = make(chan struct{})
}

// ensureTelegramProducer starts the shared ADTS producer the first time a
// Telegram stream connection is opened. The producer runs for the lifetime of
// the server, independently of any connection, so reconnects resume at the
// live edge of the stream.
func (s *Server) ensureTelegramProducer() {
	s.telegramProducer.Do(func() {
		if s.telegramRing == nil {
			s.telegramRing = newTelegramRing(telegramRingCapacity)
		}
		go s.runTelegramProducer(context.Background())
	})
}

// runTelegramProducer remuxes each track's finished HLS playlist into the
// shared ring, chaining tracks with no gap. When no track is ready it emits
// silence instead of stalling, so the consumer's ffmpeg never times out and
// reconnects to the beginning of the current track — the repeating stutter
// listeners reported.
func (s *Server) runTelegramProducer(ctx context.Context) {
	// played remembers every playlist path the producer already streamed, so
	// it never replays a track while the web player advances. Without it the
	// producer alternates between the current and next prepared tracks
	// forever, replaying both instead of padding with silence during slow
	// preloads.
	played := make([]string, 0, 8)
	alreadyPlayed := func(path string) bool {
		for _, p := range played {
			if p == path {
				return true
			}
		}
		return false
	}
	for {
		if ctx.Err() != nil {
			return
		}
		// Stream the current track once, then the next prepared track once,
		// mirroring the web player's timeline. A path already streamed (for
		// example the next track that has since become the current track) is
		// skipped, and when both queued tracks have been streamed the
		// producer pads with silence until the player advances.
		path := ""
		current := s.playbackState.CurrentHLSPlaylistPath()
		next := s.playbackState.NextHLSPlaylistPath()
		switch {
		case current != "" && !alreadyPlayed(current):
			path = current
		case next != "" && !alreadyPlayed(next):
			path = next
		}
		if path == "" {
			if err := s.appendTelegramSilence(ctx); err != nil {
				return
			}
			continue
		}
		played = append(played, path)
		if len(played) > maxTelegramPlayedTracks {
			played = append([]string(nil), played[len(played)-maxTelegramPlayedTracks:]...)
		}
		s.remuxTrackToRing(ctx, path)
	}
}

// remuxTrackToRing streams one track's ADTS into the shared ring until the
// playlist is fully consumed, the context is cancelled, or the track leaves
// the stream queue (pause or a track switch the web player already made).
func (s *Server) remuxTrackToRing(ctx context.Context, path string) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- ffmpeg.StreamHLSAsADTS(streamCtx, path, s.telegramRing)
	}()

	for {
		select {
		case err := <-done:
			if err != nil && ctx.Err() == nil {
				s.logger.Warn("Telegram ADTS remux failed", slog.String("path", path), slog.String("error", err.Error()))
				time.Sleep(500 * time.Millisecond)
			}
			return
		case <-time.After(200 * time.Millisecond):
			if !s.telegramTrackStillActive(path) {
				cancel()
				<-done
				return
			}
		}
	}
}

// appendTelegramSilence appends one frame-aligned silence chunk to the ring.
// The ring's backpressure paces the silence at real time, so at most
// silenceChunkDuration of silence is queued when the next track becomes
// available. The chunk is generated as whole ADTS frames, so repeating it
// produces a valid continuous silence stream.
func (s *Server) appendTelegramSilence(ctx context.Context) error {
	silence, err := s.telegramSilence()
	if err != nil {
		if !s.silenceWarned {
			s.silenceWarned = true
			s.logger.Warn("Telegram silence generation failed; stream may stall", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
		return nil
	}
	s.telegramRing.append(silence)
	// Pace silence at real time so a consumer that reads faster than real
	// time never sees long gaps, and no more than one chunk of silence is
	// queued when the next track becomes available.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(silenceChunkDuration):
	}
	return nil
}

// telegramSilence returns a cached ADTS silence chunk spanning
// silenceChunkDuration. Because the chunk consists of whole AAC frames,
// appending it repeatedly never splits a frame across chunk boundaries.
func (s *Server) telegramSilence() ([]byte, error) {
	s.silenceOnce.Do(func() {
		var buf bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.silenceErr = ffmpeg.GenerateSilenceADTS(ctx, &buf, silenceChunkDuration)
		s.silenceBytes = buf.Bytes()
	})
	return s.silenceBytes, s.silenceErr
}
