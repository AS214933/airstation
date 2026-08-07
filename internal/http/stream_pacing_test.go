package http

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gtmedia "github.com/annihilatorrrr/gotgcall/media"
	"github.com/cheatsnake/airstation/internal/netease"
	"github.com/cheatsnake/airstation/internal/pkg/ffmpeg"
	"github.com/cheatsnake/airstation/internal/pkg/hls"
	"github.com/cheatsnake/airstation/internal/playback"
	pmedia "github.com/pion/webrtc/v4/pkg/media"
)

// eightSecondNetEaseClient returns 8s tracks so the e2e pacing test observes
// several real track transitions.
type eightSecondNetEaseClient struct{}

func (c *eightSecondNetEaseClient) Playlist(_ string, _ string) (*netease.Playlist, error) {
	return &netease.Playlist{
		ID:   "1",
		Name: "Playlist",
		Tracks: []*netease.Song{
			{ID: 1, Name: "One", Artists: []string{"A"}, Duration: 8},
			{ID: 2, Name: "Two", Artists: []string{"B"}, Duration: 8},
			{ID: 3, Name: "Three", Artists: []string{"C"}, Duration: 8},
			{ID: 4, Name: "Four", Artists: []string{"D"}, Duration: 8},
		},
	}, nil
}

func (c *eightSecondNetEaseClient) SongURL(songID int64, _ netease.Quality, _ string) (*netease.SongURL, error) {
	return &netease.SongURL{URL: "https://example.test/" + string(rune('0'+songID)) + ".mp3", BitRate: 128}, nil
}

func (c *eightSecondNetEaseClient) Lyrics(_ int64, _ string) (*netease.Lyrics, error) {
	return nil, nil
}
func (c *eightSecondNetEaseClient) Account(_ string) (*netease.Account, error) {
	return &netease.Account{Nickname: "test"}, nil
}

// e2eHLSMaker generates fMP4 HLS playlists of the given duration. slowFrom
// makes every preparation from that call index onwards (1-based) sleep for
// slowFor, which simulates a slow NetEase download leaving the streamer
// without a prepared next track at a track boundary. Play() prepares the
// first two tracks synchronously, so slowFrom: 3 makes every preloaded track
// slow deterministically, regardless of which tracks the random picker
// chooses.
type e2eHLSMaker struct {
	duration int
	slowFrom int
	slowFor  time.Duration

	mu    sync.Mutex
	calls int
}

func (m *e2eHLSMaker) MakeRemoteHLSPlaylist(_ string, outDir, segName string, _ int, _ int) error {
	m.mu.Lock()
	m.calls++
	slow := m.slowFrom > 0 && m.calls >= m.slowFrom
	m.mu.Unlock()
	if slow {
		time.Sleep(m.slowFor)
	}
	dur := fmt.Sprintf("%d", m.duration)
	// Derive a distinct sine frequency per track so tests can tell which
	// track a consumer is hearing from the encoded bytes alone.
	freq := "440"
	for _, prefix := range []string{"netease-1", "netease-2", "netease-3", "netease-4", "netease-5", "netease-6"} {
		if strings.HasPrefix(segName, prefix+"-") {
			freq = fmt.Sprintf("%d", 300+100*len(prefix))
			break
		}
	}
	cmd := exec.Command("ffmpeg",
		"-y", "-f", "lavfi", "-i", "sine=frequency="+freq+":sample_rate=48000:duration="+dur,
		"-vn", "-c:a", "aac", "-b:a", "128k",
		"-start_number", "0", "-hls_time", dur,
		"-hls_playlist_type", "event",
		"-hls_segment_type", "fmp4",
		"-hls_fmp4_init_filename", segName+"init"+hls.InitSegmentExtension,
		"-hls_segment_filename", filepath.Join(outDir, segName+"%d"+hls.SegmentExtension),
		filepath.Join(outDir, segName+".m3u8"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("test hls generation failed: %v\n%s", err, out)
	}
	return nil
}

// sampleRecorder captures the samples handed to the WebRTC writer by
// gotgcall's paced streamer — the closest local proxy for what a listener
// actually hears.
type sampleRecorder struct {
	mu      sync.Mutex
	samples []sampleWrite
}

type sampleWrite struct {
	at   time.Time
	dur  time.Duration
	size int
}

func (r *sampleRecorder) WriteSample(s pmedia.Sample) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples = append(r.samples, sampleWrite{at: time.Now(), dur: s.Duration, size: len(s.Data)})
	return nil
}

// silenceSamples counts samples that are clearly Opus silence packets (the
// producer's padding), which are only a few bytes per 20 ms frame.
func (r *sampleRecorder) silenceSamples() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.samples {
		if s.size <= 8 {
			n++
		}
	}
	return n
}

// stats returns the sample count, silence-sample count, and maximum inter
// sample gap under a single lock.
func (r *sampleRecorder) stats() (total, silence int, maxGap time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	total = len(r.samples)
	for _, s := range r.samples {
		if s.size <= 8 {
			silence++
		}
	}
	for i := 1; i < len(r.samples); i++ {
		gap := r.samples[i].at.Sub(r.samples[i-1].at)
		if gap > maxGap {
			maxGap = gap
		}
	}
	return total, silence, maxGap
}

func (r *sampleRecorder) report(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var maxGap time.Duration
	totalStall := time.Duration(0)
	stallCount := 0
	for i := 1; i < len(r.samples); i++ {
		gap := r.samples[i].at.Sub(r.samples[i-1].at)
		if gap > maxGap {
			maxGap = gap
		}
		if gap > 250*time.Millisecond {
			totalStall += gap
			stallCount++
		}
	}
	audioMs := 0
	for _, s := range r.samples {
		audioMs += int(s.dur.Milliseconds())
	}
	t.Logf("samples=%d audio=%dms wall=%v maxGap=%v stalls>250ms=%d totalStall=%v",
		len(r.samples), audioMs, time.Since(r.samples[0].at), maxGap, stallCount, totalStall)
}

func runPacingServer(t *testing.T, maker *e2eHLSMaker, client netease.Client) (*httptest.Server, *playback.State) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := newTestStationStore()
	ns := netease.NewService(store, client, log)
	if err := ns.Load(); err != nil {
		t.Fatalf("load netease service: %v", err)
	}
	state := playback.NewStateWithHLSMaker(ns, maker, t.TempDir(), log)
	state.PlayNotify = make(chan string, 64)
	state.NewTrackNotify = make(chan string, 64)
	state.PauseNotify = make(chan bool, 64)
	if err := state.Play(); err != nil {
		t.Fatalf("start playback: %v", err)
	}
	go state.Run()

	server := &Server{playbackState: state, logger: log.WithGroup("http")}
	ts := httptest.NewServer(http.HandlerFunc(server.handleTelegramStream))
	t.Cleanup(ts.Close)
	return ts, state
}

func startConsumer(t *testing.T, url string, rec *sampleRecorder) (*gtmedia.Streamer, func()) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	src := gtmedia.FromURL(url, gtmedia.EncodeOptions{})
	streams, err := src.Open(ctx)
	if err != nil {
		cancel()
		t.Fatalf("open telegram stream source: %v", err)
	}
	fr, err := gtmedia.NewOpusFrameReader(streams.Audio)
	if err != nil {
		streams.Close()
		cancel()
		t.Fatalf("open opus frame reader: %v", err)
	}
	st := gtmedia.NewStreamer(ctx, fr, rec, log, nil)
	st.Start()
	return st, func() {
		st.Stop()
		streams.Close()
		cancel()
	}
}

func TestTelegramStreamEndToEndPacing(t *testing.T) {
	ts, _ := runPacingServer(t, &e2eHLSMaker{duration: 8}, &eightSecondNetEaseClient{})

	rec := &sampleRecorder{}
	st, stop := startConsumer(t, ts.URL+telegramStreamPath, rec)
	defer stop()

	// Observe four 8s tracks (three transitions) plus startup.
	select {
	case <-st.Done():
	case <-time.After(36 * time.Second):
	}
	rec.report(t)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.samples) < 1200 {
		t.Fatalf("too few samples received: %d", len(rec.samples))
	}
	for i := 1; i < len(rec.samples); i++ {
		gap := rec.samples[i].at.Sub(rec.samples[i-1].at)
		if gap > 500*time.Millisecond {
			t.Errorf("audible stall of %v between samples %d and %d", gap, i-1, i)
		}
	}
}

// sixTrackNetEaseClient returns six 6s tracks so the deterministic
// slow-preload test has a track pool that never runs out of candidates while
// the slow downloads are in flight.
type sixTrackNetEaseClient struct{}

func (c *sixTrackNetEaseClient) Playlist(_ string, _ string) (*netease.Playlist, error) {
	songs := make([]*netease.Song, 0, 6)
	for id := int64(1); id <= 6; id++ {
		songs = append(songs, &netease.Song{
			ID:       id,
			Name:     []string{"One", "Two", "Three", "Four", "Five", "Six"}[id-1],
			Artists:  []string{"A"},
			Duration: 6,
		})
	}
	return &netease.Playlist{ID: "1", Name: "Playlist", Tracks: songs}, nil
}

func (c *sixTrackNetEaseClient) SongURL(songID int64, _ netease.Quality, _ string) (*netease.SongURL, error) {
	return &netease.SongURL{URL: "https://example.test/" + string(rune('0'+songID)) + ".mp3", BitRate: 128}, nil
}

func (c *sixTrackNetEaseClient) Lyrics(_ int64, _ string) (*netease.Lyrics, error) { return nil, nil }
func (c *sixTrackNetEaseClient) Account(_ string) (*netease.Account, error) {
	return &netease.Account{Nickname: "test"}, nil
}

// TestTelegramStreamSlowPreloadStaysContinuous reproduces the reported
// "choppy" Telegram audio. Every track beyond the first two takes 20s to
// prepare (slow NetEase download), so the producer runs out of prepared
// tracks at the first boundary. It must keep the stream flowing with silence
// instead of stalling: a stalled stream drains the consumer's buffer, times
// out gotgcall's ffmpeg (10s socket timeout), and restarts the current track
// from the beginning — the repeating-stutter loop listeners reported.
func TestTelegramStreamSlowPreloadStaysContinuous(t *testing.T) {
	ts, _ := runPacingServer(t, &e2eHLSMaker{
		duration: 6,
		slowFrom: 3,
		slowFor:  20 * time.Second,
	}, &sixTrackNetEaseClient{})

	rec := &sampleRecorder{}
	st, stop := startConsumer(t, ts.URL+telegramStreamPath, rec)
	defer stop()

	// Covers both stall windows: 12s-20s (first slow preload) and 26s-40s
	// (second slow preload).
	select {
	case <-st.Done():
	case <-time.After(48 * time.Second):
	}
	rec.report(t)

	total, silence, maxGap := rec.stats()
	if total < 2000 {
		t.Fatalf("stream ended early: only %d samples received", total)
	}
	if silence < 100 {
		t.Errorf("expected the producer to pad the stream with silence during slow preloads, got %d silence samples", silence)
	}
	t.Logf("maxGap=%v silenceSamples=%d", maxGap, silence)
	if maxGap > time.Second {
		t.Errorf("listener-audible gap of %v during slow preload", maxGap)
	}
}

// TestTelegramStreamReconnectDoesNotReplay verifies that a reconnecting
// consumer resumes at the live edge of the shared stream instead of replaying
// the current track from the beginning. The per-connection handler used to
// serve the current track from byte 0 to every new connection, so any
// reconnect (ffmpeg timeout, service restart) repeated the song from the
// start — the audible "stutter" reported by listeners.
func TestTelegramStreamReconnectDoesNotReplay(t *testing.T) {
	ts, state := runPacingServer(t, &e2eHLSMaker{duration: 6}, &sixTrackNetEaseClient{})

	// Keep a real consumer attached so the producer stays paced; let the web
	// state advance to track 4, then capture track 4's starting bytes.
	rec := &sampleRecorder{}
	_, stop := startConsumer(t, ts.URL+telegramStreamPath, rec)
	defer stop()

	deadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(deadline) {
		if state.CurrentTrack != nil && state.CurrentTrack.Name == "Four" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if state.CurrentTrack == nil || state.CurrentTrack.Name != "Four" {
		t.Fatalf("playback never reached track 4 (current=%v)", state.CurrentTrack)
	}
	time.Sleep(1500 * time.Millisecond) // mid-track-4 on the web timeline

	trackStart := trackStartBytes(t, state.CurrentHLSPlaylistPath())

	// Stop the first consumer, then reconnect: the new connection must not
	// hand out track 4's beginning again.
	stop()
	time.Sleep(time.Second)

	first := readStreamBytes(t, ts.URL+telegramStreamPath, len(trackStart))
	if bytes.Equal(first, trackStart) {
		t.Fatalf("reconnected stream replayed the current track from its start (%d bytes)", len(first))
	}
	t.Logf("reconnected stream differs from track start (no replay)")
}

// trackStartBytes remuxes the track playlist to ADTS and returns the first
// len bytes, which is exactly what a replay-from-the-beginning would serve.
func trackStartBytes(t *testing.T, path string) []byte {
	t.Helper()
	var buf bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := ffmpeg.StreamHLSAsADTS(ctx, path, &buf); err != nil {
		t.Fatalf("remux track for start bytes: %v", err)
	}
	data := buf.Bytes()
	if len(data) < 4096 {
		t.Fatalf("track ADTS too short: %d bytes", len(data))
	}
	return data[:4096]
}

// readStreamBytes reads the telegram stream until n bytes are received.
func readStreamBytes(t *testing.T, url string, n int) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request telegram stream: %v", err)
	}
	defer resp.Body.Close()
	data := make([]byte, 0, n)
	buf := make([]byte, 4096)
	for len(data) < n {
		m, rerr := resp.Body.Read(buf)
		if m > 0 {
			data = append(data, buf[:m]...)
		}
		if rerr != nil {
			break
		}
	}
	if len(data) < n {
		t.Fatalf("stream closed early: got %d of %d bytes", len(data), n)
	}
	// A live-edge subscriber can start mid-frame; ffmpeg re-syncs within a
	// few hundred bytes, so just require ADTS data somewhere near the start.
	synced := false
	for i := 0; i+1 < len(data) && i < 512; i++ {
		if data[i] == 0xFF && data[i+1]&0xF6 == 0xF0 {
			synced = true
			break
		}
	}
	if !synced {
		t.Fatalf("reconnected stream contains no ADTS sync in the first 512 bytes: % x", data[:min(16, len(data))])
	}
	return data
}
