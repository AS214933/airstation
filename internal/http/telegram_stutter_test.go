package http

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// modelsOpusFrameMs is the duration of one Opus frame the gotgcall consumer
// hands to the WebRTC writer.
const modelsOpusFrameMs = 20

// TestTelegramStreamNoMidTrackSilenceBursts drives the real gotgcall consumer
// (HTTP -> ffmpeg -> Opus -> paced streamer) over multi-segment tracks and
// fails if the producer injects a multi-second silence run in the middle of
// the stream. Regression for the stutter where the producer remuxed queued
// tracks at full speed, raced ahead of the web player's timeline, and then
// padded silence until the player advanced. All six prepared tracks play
// back-to-back here, so a silence run followed by music can only be the
// producer racing ahead; only the trailing run after the last track is
// legitimate.
func TestTelegramStreamNoMidTrackSilenceBursts(t *testing.T) {
	ts, _ := runPacingServer(t, &e2eHLSMaker{duration: 6, segDuration: 2}, &sixTrackNetEaseClient{})
	rec := &sampleRecorder{}
	st, stop := startConsumer(t, ts.URL+telegramStreamPath, rec)
	defer stop()
	select {
	case <-st.Done():
	case <-time.After(45 * time.Second):
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()

	// Runs of consecutive silence samples (Opus silence = a few bytes).
	longestSilenceRun := 0
	silenceRun := 0
	runStart := -1
	midStreamBursts := 0
	for i, s := range rec.samples {
		if s.size <= 8 {
			if silenceRun == 0 {
				runStart = i
			}
			silenceRun++
			if silenceRun > longestSilenceRun {
				longestSilenceRun = silenceRun
			}
			continue
		}
		if silenceRun >= 25 { // >= 500ms of silence, followed by music
			t.Errorf("mid-stream silence burst: %dms (samples %d..%d) at t=%.1fs",
				silenceRun*modelsOpusFrameMs, runStart, i-1, rec.samples[runStart].at.Sub(rec.samples[0].at).Seconds())
			midStreamBursts++
		}
		silenceRun = 0
	}
	if midStreamBursts == 0 {
		t.Logf("no mid-stream silence bursts (longest overall run %dms)", longestSilenceRun*modelsOpusFrameMs)
	}
	if silenceRun >= 25 {
		t.Logf("trailing silence run (playlist exhausted): %dms at t=%.1fs",
			silenceRun*modelsOpusFrameMs, rec.samples[runStart].at.Sub(rec.samples[0].at).Seconds())
	}
}

// TestTelegramStreamMultiSegmentNoContentGaps decodes the captured ADTS to
// PCM and looks for silence gaps inside the music. The test tracks are pure
// sine waves, so decoded silence longer than 50ms is a real glitch (dropped
// frames at an intra-track segment boundary), not a quiet passage.
func TestTelegramStreamMultiSegmentNoContentGaps(t *testing.T) {
	ts, _ := runPacingServer(t, &e2eHLSMaker{duration: 8, segDuration: 2}, &durationNetEaseClient{duration: 8})
	raw := captureRawADTS(t, readStreamBody(t, ts.URL+telegramStreamPath), 20*time.Second)
	adts, err := os.CreateTemp(t.TempDir(), "capture-*.adts")
	if err != nil {
		t.Fatalf("create capture: %v", err)
	}
	if _, err := adts.Write(raw); err != nil {
		t.Fatalf("write capture: %v", err)
	}
	adts.Close()

	cmd := exec.Command("ffmpeg", "-hide_banner", "-nostdin", "-i", adts.Name(),
		"-af", "silencedetect=noise=-45dB:d=0.05", "-f", "null", "-")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("decode capture: %v\n%s", err, out)
	}
	// silencedetect prints "silence_start: X" / "silence_end: Y" lines.
	gaps := 0
	start, end := "", ""
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "silence_start") {
			gaps++
			start = strings.TrimSpace(line)
		}
		if strings.Contains(line, "silence_end") {
			end = strings.TrimSpace(line)
			t.Logf("content gap: %s -> %s", start, end)
			start, end = "", ""
		}
	}
	if start != "" {
		t.Logf("content gap (unterminated): %s", start)
	}
	t.Logf("adts=%d bytes, decoded silence gaps >50ms: %d", len(raw), gaps)
	if gaps > 0 {
		t.Errorf("content-level gaps in decoded telegram ADTS: %d", gaps)
	}
}

// generateADTS renders n seconds of 48 kHz mono sine as ADTS and returns the
// bytes.
func generateADTS(t *testing.T, seconds int) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tone.adts")
	cmd := exec.Command("ffmpeg", "-y", "-f", "lavfi",
		"-i", fmt.Sprintf("sine=frequency=440:sample_rate=48000:duration=%d", seconds),
		"-vn", "-c:a", "aac", "-b:a", "128k", "-f", "adts", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate adts: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read adts: %v", err)
	}
	return raw
}

// TestPacedWriterRate verifies the pacedWriter delivers audio at roughly real
// time (telegramProducerRate x) instead of racing a whole track in a burst.
func TestPacedWriterRate(t *testing.T) {
	raw := generateADTS(t, 3)
	start := time.Now()
	if _, err := (&pacedWriter{w: io.Discard, maxRate: telegramProducerRate}).Write(raw); err != nil {
		t.Fatalf("write: %v", err)
	}
	elapsed := time.Since(start)
	audio := 3 * time.Second
	want := time.Duration(float64(audio) / telegramProducerRate)
	t.Logf("audio=%v elapsed=%v want≈%v", audio, elapsed, want)
	if elapsed < want*3/4 {
		t.Fatalf("paced writer raced: %v for 3s of audio (expected ~%v)", elapsed, want)
	}
	if elapsed > want*2 {
		t.Fatalf("paced writer too slow: %v for 3s of audio (expected ~%v)", elapsed, want)
	}
}

// TestPacedWriterChunkedFrames writes ADTS in sub-frame chunks so the writer
// must carry partial frames across writes, and verifies every frame reaches
// the underlying writer intact and in order.
func TestPacedWriterChunkedFrames(t *testing.T) {
	raw := generateADTS(t, 2)
	wantFrames, tail := splitADTSFrames(raw)
	if len(tail) != 0 {
		t.Fatalf("whole-buffer split left %d trailing bytes", len(tail))
	}
	var out bytes.Buffer
	pw := &pacedWriter{w: &out, maxRate: 100} // fast, so the test stays short
	step := 100
	for i := 0; i < len(raw); i += step {
		end := min(i+step, len(raw))
		if _, err := pw.Write(raw[i:end]); err != nil {
			t.Fatalf("chunked write: %v", err)
		}
	}
	gotFrames, tail := splitADTSFrames(out.Bytes())
	if len(tail) != 0 {
		t.Fatalf("captured output left %d trailing bytes", len(tail))
	}
	if len(gotFrames) != len(wantFrames) {
		t.Fatalf("frame count: got %d want %d", len(gotFrames), len(wantFrames))
	}
	for i := range wantFrames {
		if !bytes.Equal(gotFrames[i], wantFrames[i]) {
			t.Fatalf("frame %d differs", i)
		}
	}
}

// TestAdtsFrameRate checks the frame-duration math the pacer relies on for
// both sample rates used by the test tracks.
func TestAdtsFrameRate(t *testing.T) {
	raw := generateADTS(t, 1)
	frames, _ := splitADTSFrames(raw)
	if len(frames) == 0 {
		t.Fatalf("no frames in generated adts")
	}
	if got := adtsFrameRate(frames[0]); got != 1024*time.Second/48000 {
		t.Fatalf("48k frame rate = %v, want %v", got, 1024*time.Second/48000)
	}
	// Patch the sampling-frequency index to 44100 Hz and check the rate.
	f := append([]byte(nil), frames[0]...)
	f[2] = (f[2] & 0xC3) | (4 << 2) // index 4 = 44100 Hz
	if got := adtsFrameRate(f); got != 1024*time.Second/44100 {
		t.Fatalf("44.1k frame rate = %v, want %v", got, 1024*time.Second/44100)
	}
}

// captureRawADTS reads the stream as fast as possible and returns the raw
// ADTS bytes, so delivery timing is irrelevant: any gap in the decoded audio
// is a content-level glitch in the produced stream.
func captureRawADTS(t *testing.T, body io.Reader, forDuration time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(forDuration)
	var out []byte
	header := make([]byte, 7)
	payload := make([]byte, 2048)
	for time.Now().Before(deadline) {
		if _, err := io.ReadFull(body, header); err != nil {
			return out
		}
		length := int(header[3]&0x03)<<11 | int(header[4])<<3 | int(header[5]>>5)&0x07
		if length-7 > len(payload) {
			payload = make([]byte, length)
		}
		if _, err := io.ReadFull(body, payload[:length-7]); err != nil {
			return out
		}
		out = append(out, header...)
		out = append(out, payload[:length-7]...)
	}
	return out
}

// readStreamBody opens the telegram stream and returns its response body.
func readStreamBody(t *testing.T, url string) io.Reader {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("request telegram stream: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp.Body
}
