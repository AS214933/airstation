package http

import (
	"io"
	"sync"
	"time"
)

// telegramProducerRate is the maximum audio seconds the Telegram producer may
// write per wall-clock second. Keeping it slightly above real time gives the
// ring a small head start at track boundaries, while staying well below 2x so
// the producer can never finish both queued tracks before the web player
// advances (which previously made it pad multi-second silence).
const telegramProducerRate = 1.05

// adtsSampleRates maps the ADTS sampling-frequency index to Hz.
var adtsSampleRates = [16]int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// pacedWriter caps the producer's track-remux write rate at real time. The
// ring's backpressure only paces the producer while a consumer is attached and
// reading; with no reader (or one that drains fast into its own buffers) the
// producer used to remux whole tracks in milliseconds, race ahead of the web
// player's timeline, and then pad silence until the player caught up. Pacing
// by audio time keeps the producer locked to the player's timeline in every
// case; the ring's backlog and the consumer's buffers then absorb the small
// remux-startup gap between tracks.
//
// The writer splits each incoming chunk into single ADTS frames and paces
// between frames, so ffmpeg's bursty pipe writes are delivered to the ring
// smoothly instead of arriving as one large burst after a long sleep.
type pacedWriter struct {
	w       io.Writer
	maxRate float64

	mu    sync.Mutex
	start time.Time
	audio time.Duration
	carry []byte // partial ADTS frame from the previous write
}

func (p *pacedWriter) Write(buf []byte) (int, error) {
	data := make([]byte, 0, len(p.carry)+len(buf))
	data = append(data, p.carry...)
	data = append(data, buf...)
	frames, tail := splitADTSFrames(data)
	p.carry = tail

	for _, frame := range frames {
		p.mu.Lock()
		if p.start.IsZero() {
			p.start = time.Now()
		}
		p.audio += adtsFrameRate(frame)
		target := time.Duration(float64(p.audio) / p.maxRate)
		p.mu.Unlock()

		if wait := target - time.Since(p.start); wait > 0 {
			time.Sleep(wait)
		}
		if _, err := p.w.Write(frame); err != nil {
			return 0, err
		}
	}
	return len(buf), nil
}

// splitADTSFrames splits b into whole ADTS frames. Any trailing partial frame
// is returned separately so the caller can prepend it to the next chunk.
func splitADTSFrames(b []byte) ([][]byte, []byte) {
	var frames [][]byte
	i := 0
	for i < len(b) {
		if i+7 > len(b) {
			return frames, b[i:]
		}
		if b[i] != 0xFF || b[i+1]&0xF6 != 0xF0 {
			i++
			continue
		}
		length := int(b[i+3]&0x03)<<11 | int(b[i+4])<<3 | int(b[i+5]>>5)&0x07
		if length < 7 || i+length > len(b) {
			return frames, b[i:]
		}
		frames = append(frames, b[i:i+length])
		i += length
	}
	return frames, nil
}

// adtsFrameRate returns the audio duration of a single ADTS frame: 1024
// samples at the sample rate encoded in its header.
func adtsFrameRate(frame []byte) time.Duration {
	if len(frame) < 7 {
		return 0
	}
	srIdx := int(frame[2]>>2) & 0x0F
	if srIdx >= len(adtsSampleRates) {
		return 0
	}
	return time.Duration(1024 * time.Second / time.Duration(adtsSampleRates[srIdx]))
}

// adtsFrameDuration sums the audio duration of every complete ADTS frame in b
// and returns the count together with any trailing partial frame. Kept for
// tests and diagnostics.
func adtsFrameDuration(b []byte) (time.Duration, []byte) {
	frames, tail := splitADTSFrames(b)
	var total time.Duration
	for _, f := range frames {
		total += adtsFrameRate(f)
	}
	return total, tail
}
