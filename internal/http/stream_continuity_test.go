package http

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/cheatsnake/airstation/internal/netease"
	"github.com/cheatsnake/airstation/internal/pkg/hls"
	"github.com/cheatsnake/airstation/internal/playback"
)

// shortTrackNetEaseClient returns short tracks so the continuity test observes
// several track transitions quickly.
type shortTrackNetEaseClient struct{}

func (c *shortTrackNetEaseClient) Playlist(_ string, _ string) (*netease.Playlist, error) {
	return &netease.Playlist{
		ID:   "1",
		Name: "Playlist",
		Tracks: []*netease.Song{
			{ID: 1, Name: "One", Artists: []string{"A"}, Duration: 3},
			{ID: 2, Name: "Two", Artists: []string{"B"}, Duration: 3},
			{ID: 3, Name: "Three", Artists: []string{"C"}, Duration: 3},
		},
	}, nil
}

func (c *shortTrackNetEaseClient) SongURL(songID int64, _ netease.Quality, _ string) (*netease.SongURL, error) {
	return &netease.SongURL{URL: "https://example.test/" + string(rune('0'+songID)) + ".mp3", BitRate: 128}, nil
}

func (c *shortTrackNetEaseClient) Lyrics(_ int64, _ string) (*netease.Lyrics, error) { return nil, nil }
func (c *shortTrackNetEaseClient) Account(_ string) (*netease.Account, error) {
	return &netease.Account{Nickname: "test"}, nil
}

// continuityHL SMaker generates HLS playlists like realHLSMaker but takes a
// delay hook so the test can slow down track preparation.
type continuityHLSMaker struct {
	prepareDelay func(segName string)
}

func (m *continuityHLSMaker) MakeRemoteHLSPlaylist(_ string, outDir, segName string, _ int, _ int) error {
	if m.prepareDelay != nil {
		m.prepareDelay(segName)
	}
	cmd := exec.Command("ffmpeg",
		"-y", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=3",
		"-vn", "-c:a", "aac", "-b:a", "128k",
		"-start_number", "0", "-hls_time", "3",
		"-hls_playlist_type", "event",
		"-hls_segment_type", "fmp4",
		"-hls_fmp4_init_filename", segName+"init"+hls.InitSegmentExtension,
		"-hls_segment_filename", filepath.Join(outDir, segName+"%d"+hls.SegmentExtension),
		filepath.Join(outDir, segName+".m3u8"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return &exec.ExitError{Stderr: out}
	}
	return nil
}

// adtsFrame is one parsed ADTS frame.
type adtsFrame struct {
	length int
	at     time.Time
}

// parseADTSStream reads frames from the telegram stream for the given duration
// and returns them with arrival timestamps.
func parseADTSStream(t *testing.T, body io.Reader, forDuration time.Duration) []adtsFrame {
	t.Helper()
	deadline := time.Now().Add(forDuration)
	var frames []adtsFrame
	payload := make([]byte, 2048)
	header := make([]byte, 7)
	for time.Now().Before(deadline) {
		if _, err := io.ReadFull(body, header); err != nil {
			return frames
		}
		if header[0] != 0xFF || header[1]&0xF6 != 0xF0 {
			t.Fatalf("invalid ADTS sync word: % x", header)
		}
		length := int(header[3]&0x03)<<11 | int(header[4])<<3 | int(header[5]>>5)&0x07
		if length < 7 {
			t.Fatalf("invalid ADTS frame length %d", length)
		}
		if length-7 > len(payload) {
			payload = make([]byte, length)
		}
		if _, err := io.ReadFull(body, payload[:length-7]); err != nil {
			return frames
		}
		frames = append(frames, adtsFrame{length: length, at: time.Now()})
	}
	return frames
}

// reportGaps summarizes the inter-frame arrival gaps of the ADTS stream.
func reportGaps(t *testing.T, frames []adtsFrame) {
	t.Helper()
	var maxGap time.Duration
	var gapsOver time.Duration
	gapCount := 0
	for i := 1; i < len(frames); i++ {
		gap := frames[i].at.Sub(frames[i-1].at)
		if gap > maxGap {
			maxGap = gap
		}
		if gap > 300*time.Millisecond {
			t.Logf("gap@frame %d: %v (size %d -> %d)", i, gap.Round(time.Millisecond), frames[i-1].length, frames[i].length)
			gapsOver += gap
			gapCount++
		}
	}
	t.Logf("frames=%d maxGap=%v gaps>300ms=%d totalStall=%v", len(frames), maxGap, gapCount, gapsOver)
}

func TestTelegramStreamContinuityAcrossTracks(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := newTestStationStore()
	ns := netease.NewService(store, &shortTrackNetEaseClient{}, log)
	if err := ns.Load(); err != nil {
		t.Fatalf("load netease service: %v", err)
	}

	state := playback.NewStateWithHLSMaker(ns, &continuityHLSMaker{}, t.TempDir(), log)
	state.PlayNotify = make(chan string, 1)
	state.NewTrackNotify = make(chan string, 1)
	state.PauseNotify = make(chan bool, 1)
	if err := state.Play(); err != nil {
		t.Fatalf("start playback: %v", err)
	}
	go state.Run()

	server := &Server{playbackState: state, logger: log.WithGroup("http")}
	ts := httptest.NewServer(http.HandlerFunc(server.handleTelegramStream))
	defer ts.Close()

	resp, err := http.Get(ts.URL + telegramStreamPath)
	if err != nil {
		t.Fatalf("request telegram stream: %v", err)
	}
	defer resp.Body.Close()

	// Observe ~4 track transitions.
	frames := parseADTSStream(t, resp.Body, 16*time.Second)
	reportGaps(t, frames)
}
