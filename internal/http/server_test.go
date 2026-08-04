package http

import (
	"context"
	"fmt"
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
	"github.com/cheatsnake/airstation/internal/station"
)

func TestTelegramStreamURL(t *testing.T) {
	got := telegramStreamURL("7331")
	const want = "http://127.0.0.1:7331/telegram-stream"
	if got != want {
		t.Fatalf("telegramStreamURL() = %q, want %q", got, want)
	}
}

type testStationStore struct {
	props map[string]string
}

func newTestStationStore() *testStationStore {
	return &testStationStore{props: map[string]string{
		"netease_playlist_url": "1",
		"netease_quality":      string(netease.QualityStandard),
	}}
}

func (s *testStationStore) StationProperties() ([]*station.Property, error) {
	props := make([]*station.Property, 0, len(s.props))
	for key, value := range s.props {
		props = append(props, &station.Property{Key: key, Value: value})
	}
	return props, nil
}

func (s *testStationStore) UpsertStationProperty(key, value string) (*station.Property, error) {
	s.props[key] = value
	return &station.Property{Key: key, Value: value}, nil
}

func (s *testStationStore) DeleteStationProperty(key string) error {
	delete(s.props, key)
	return nil
}

type testNetEaseClient struct{}

func (c *testNetEaseClient) Playlist(_ string, _ string) (*netease.Playlist, error) {
	return &netease.Playlist{
		ID:   "1",
		Name: "Playlist",
		Tracks: []*netease.Song{
			{ID: 1, Name: "One", Artists: []string{"A"}, Duration: 10},
			{ID: 2, Name: "Two", Artists: []string{"B"}, Duration: 10},
			{ID: 3, Name: "Three", Artists: []string{"C"}, Duration: 10},
		},
	}, nil
}

func (c *testNetEaseClient) SongURL(songID int64, _ netease.Quality, _ string) (*netease.SongURL, error) {
	return &netease.SongURL{URL: "https://example.test/" + string(rune('0'+songID)) + ".mp3", BitRate: 128}, nil
}

func (c *testNetEaseClient) Lyrics(_ int64, _ string) (*netease.Lyrics, error) {
	return nil, nil
}

func (c *testNetEaseClient) Account(_ string) (*netease.Account, error) {
	return &netease.Account{Nickname: "test"}, nil
}

// realHLSMaker generates an actual fMP4 HLS playlist for the prepared track,
// so the Telegram stream handler has a real m3u8 to remux.
type realHLSMaker struct{}

func (m *realHLSMaker) MakeRemoteHLSPlaylist(_ string, outDir, segName string, _ int, _ int) error {
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
		return fmt.Errorf("test hls generation failed: %v\n%s", err, out)
	}
	return nil
}

func TestHandleTelegramStream(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := newTestStationStore()
	netEaseService := netease.NewService(store, &testNetEaseClient{}, log)
	if err := netEaseService.Load(); err != nil {
		t.Fatalf("load netease service: %v", err)
	}

	state := playback.NewStateWithHLSMaker(netEaseService, &realHLSMaker{}, t.TempDir(), log)
	state.PlayNotify = make(chan string, 1)
	state.NewTrackNotify = make(chan string, 1)
	state.PauseNotify = make(chan bool, 1)
	if err := state.Play(); err != nil {
		t.Fatalf("start playback: %v", err)
	}

	server := &Server{playbackState: state, logger: log.WithGroup("http")}
	ts := httptest.NewServer(http.HandlerFunc(server.handleTelegramStream))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+telegramStreamPath, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request telegram stream: %v", err)
	}
	defer resp.Body.Close()

	deadline := time.Now().Add(10 * time.Second)
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if buf[0] != 0xFF || buf[1]&0xF6 != 0xF0 {
				t.Fatalf("stream does not start with an ADTS sync word: % x", buf[:min(n, 8)])
			}
			// ADTS frames are flowing; stop the stream and let the handler unwind.
			cancel()
			return
		}
		if readErr != nil {
			t.Fatalf("telegram stream closed early: %v", readErr)
		}
	}
	cancel()
	t.Fatal("timed out waiting for ADTS audio in telegram stream")
}
