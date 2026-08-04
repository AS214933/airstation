package playback

import (
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cheatsnake/airstation/internal/netease"
	"github.com/cheatsnake/airstation/internal/pkg/hls"
	"github.com/cheatsnake/airstation/internal/station"
)

type stateStationStore struct {
	props map[string]string
}

func newStateStationStore() *stateStationStore {
	return &stateStationStore{
		props: map[string]string{
			"netease_playlist_url": "1",
			"netease_quality":      string(netease.QualityStandard),
		},
	}
}

func (s *stateStationStore) StationProperties() ([]*station.Property, error) {
	props := make([]*station.Property, 0, len(s.props))
	for key, value := range s.props {
		props = append(props, &station.Property{Key: key, Value: value})
	}
	return props, nil
}

func (s *stateStationStore) UpsertStationProperty(key, value string) (*station.Property, error) {
	s.props[key] = value
	return &station.Property{Key: key, Value: value}, nil
}

func (s *stateStationStore) DeleteStationProperty(key string) error {
	delete(s.props, key)
	return nil
}

type stateNetEaseClient struct {
	playlist     *netease.Playlist
	mutex        sync.Mutex
	songURLCalls map[int64]int
}

func (c *stateNetEaseClient) Playlist(_ string, _ string) (*netease.Playlist, error) {
	return c.playlist, nil
}

func (c *stateNetEaseClient) SongURL(songID int64, _ netease.Quality, _ string) (*netease.SongURL, error) {
	c.mutex.Lock()
	if c.songURLCalls == nil {
		c.songURLCalls = make(map[int64]int)
	}
	c.songURLCalls[songID]++
	call := c.songURLCalls[songID]
	c.mutex.Unlock()

	return &netease.SongURL{
		URL:     "https://example.test/" + strconv.FormatInt(songID, 10) + ".mp3?attempt=" + strconv.Itoa(call),
		BitRate: 128,
	}, nil
}

func (c *stateNetEaseClient) SongURLCalls(songID int64) int {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.songURLCalls[songID]
}

func (c *stateNetEaseClient) Lyrics(songID int64, _ string) (*netease.Lyrics, error) {
	return &netease.Lyrics{SongID: songID, Kind: "none"}, nil
}

func (c *stateNetEaseClient) Account(_ string) (*netease.Account, error) {
	return &netease.Account{}, nil
}

type stateHLSMaker struct {
	mutex sync.Mutex
	calls int
}

func (m *stateHLSMaker) MakeRemoteHLSPlaylist(_ string, _ string, _ string, _ int, _ int) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.calls++
	return nil
}

func (m *stateHLSMaker) Calls() int {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.calls
}

type flakyStateHLSMaker struct {
	mutex    sync.Mutex
	failures int
	calls    int
	called   chan int
	urls     []string
}

func (m *flakyStateHLSMaker) MakeRemoteHLSPlaylist(trackURL string, _ string, _ string, _ int, _ int) error {
	m.mutex.Lock()
	m.calls++
	call := m.calls
	m.urls = append(m.urls, trackURL)
	shouldFail := call <= m.failures
	m.mutex.Unlock()

	m.called <- call
	if shouldFail {
		return errors.New("temporary remote input failure")
	}
	return nil
}

func (m *flakyStateHLSMaker) Calls() int {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.calls
}

func (m *flakyStateHLSMaker) URLs() []string {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return append([]string(nil), m.urls...)
}

type controlledStateHLSMaker struct {
	mutex    sync.Mutex
	calls    int
	called   chan int
	releases []<-chan struct{}
}

func (m *controlledStateHLSMaker) MakeRemoteHLSPlaylist(_ string, _ string, _ string, _ int, _ int) error {
	m.mutex.Lock()
	m.calls++
	call := m.calls
	release := m.releases[call-1]
	m.mutex.Unlock()

	m.called <- call
	<-release
	return nil
}

type stateRetryWait struct {
	delay   time.Duration
	release chan struct{}
}

func TestState_LoadNextTrackUsesPreloadedSegmentsAndMetadata(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := newStateStationStore()
	netEaseService := netease.NewService(store, &stateNetEaseClient{
		playlist: &netease.Playlist{
			ID:   "1",
			Name: "Playlist",
			Tracks: []*netease.Song{
				{ID: 1, Name: "One", Artists: []string{"Artist A"}, Duration: 10},
				{ID: 2, Name: "Two", Artists: []string{"Artist B"}, Duration: 10},
				{ID: 3, Name: "Three", Artists: []string{"Artist C"}, Duration: 10},
			},
		},
	}, log)
	if err := netEaseService.Load(); err != nil {
		t.Fatalf("load netease service: %v", err)
	}

	hlsMaker := &stateHLSMaker{}
	state := NewStateWithHLSMaker(netEaseService, hlsMaker, t.TempDir(), log)
	current := stateTrack(1, "One", "Artist A", "current-seg-", 10)
	next := stateTrack(2, "Two", "Artist B", "next-seg-", 10)
	following := stateTrack(3, "Three", "Artist C", "following-seg-", 10)
	state.CurrentTrack = current.track
	state.CurrentNetEaseID = current.songID
	state.nextPrepared = next
	state.followingPrepared = following
	state.CurrentTrackElapsed = current.track.Duration
	state.IsPlaying = true
	state.playlist = hls.NewPlaylist(current.segments, next.segments)
	state.PlaylistStr = state.playlist.Generate(0)

	callsBefore := hlsMaker.Calls()
	trackName, songID, err := state.loadNextTrackLocked()
	if err != nil {
		t.Fatalf("load next track: %v", err)
	}

	if trackName != "Two - Artist B" {
		t.Fatalf("track name = %q, want %q", trackName, "Two - Artist B")
	}
	if state.CurrentTrack.Name != "Two" || state.CurrentTrack.Artist != "Artist B" {
		t.Fatalf("current track = %#v", state.CurrentTrack)
	}
	if state.CurrentNetEaseID != 2 {
		t.Fatalf("current netease id = %d, want 2", state.CurrentNetEaseID)
	}
	if songID != 2 {
		t.Fatalf("loaded song id = %d, want 2", songID)
	}
	if state.nextPrepared == nil || state.nextPrepared.songID != 3 {
		t.Fatalf("next prepared = %#v, want song 3", state.nextPrepared)
	}
	state.recordPlayedSong(songID)
	recent := recentNetEaseSongIDs(store)
	if len(recent) != 1 || recent[0] != 2 {
		t.Fatalf("recent netease song ids = %#v, want [2]", recent)
	}
	if hlsMaker.Calls() != callsBefore {
		t.Fatalf("loadNextTrackLocked called HLS maker: before=%d after=%d", callsBefore, hlsMaker.Calls())
	}

	playlist := state.playlist.Generate(0)
	if !strings.Contains(playlist, "next-seg-0"+hls.SegmentExtension) {
		t.Fatalf("playlist did not switch to preloaded next segments:\n%s", playlist)
	}
	if !strings.Contains(playlist, "following-seg-0"+hls.SegmentExtension) {
		t.Fatalf("playlist did not carry following preloaded segments:\n%s", playlist)
	}
}

func TestState_PlayRecordsOnlyCurrentSong(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := newStateStationStore()
	netEaseService := netease.NewService(store, &stateNetEaseClient{
		playlist: &netease.Playlist{
			ID:   "1",
			Name: "Playlist",
			Tracks: []*netease.Song{
				{ID: 1, Name: "One", Artists: []string{"Artist A"}, Duration: 10},
				{ID: 2, Name: "Two", Artists: []string{"Artist B"}, Duration: 10},
				{ID: 3, Name: "Three", Artists: []string{"Artist C"}, Duration: 10},
			},
		},
	}, log)
	if err := netEaseService.Load(); err != nil {
		t.Fatalf("load netease service: %v", err)
	}

	state := NewStateWithHLSMaker(netEaseService, &stateHLSMaker{}, t.TempDir(), log)
	state.PlayNotify = make(chan string, 1)

	if err := state.Play(); err != nil {
		t.Fatalf("play: %v", err)
	}

	recent := recentNetEaseSongIDs(store)
	if len(recent) != 1 || recent[0] != state.CurrentNetEaseID {
		t.Fatalf("recent netease song ids = %#v, want current song %d only", recent, state.CurrentNetEaseID)
	}
	if state.nextPrepared == nil {
		t.Fatal("next track was not prepared")
	}
	if state.nextPrepared.songID == state.CurrentNetEaseID {
		t.Fatalf("next prepared song duplicates current song %d", state.CurrentNetEaseID)
	}
	waitForPlaybackState(t, state, func(state *State) bool { return !state.preloadInFlight })
}

func TestState_PreloadRetriesUntilTemporaryFailureRecovers(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := newStateStationStore()
	client := &stateNetEaseClient{playlist: stateNetEasePlaylist()}
	netEaseService := netease.NewService(store, client, log)
	if err := netEaseService.Load(); err != nil {
		t.Fatalf("load netease service: %v", err)
	}

	hlsMaker := &flakyStateHLSMaker{failures: 2, called: make(chan int, 3)}
	state := NewStateWithHLSMaker(netEaseService, hlsMaker, t.TempDir(), log)
	current := stateTrack(1, "One", "Artist A", "current-seg-", 10)
	next := stateTrack(2, "Two", "Artist B", "next-seg-", 10)
	state.CurrentTrack = current.track
	state.CurrentNetEaseID = current.songID
	state.nextPrepared = next
	state.IsPlaying = true
	state.playlist = hls.NewPlaylist(current.segments, next.segments)
	state.PlaylistStr = state.playlist.Generate(0)

	retryWaits := make(chan stateRetryWait, 2)
	state.preloadRetryWait = func(delay time.Duration) {
		wait := stateRetryWait{delay: delay, release: make(chan struct{})}
		retryWaits <- wait
		<-wait.release
	}

	state.ensurePreloaded()
	for attempt := 1; attempt <= 2; attempt++ {
		select {
		case call := <-hlsMaker.called:
			if call != attempt {
				t.Fatalf("HLS call = %d, want %d", call, attempt)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for HLS call %d", attempt)
		}

		select {
		case wait := <-retryWaits:
			if wait.delay != preloadRetryDelay(attempt) {
				t.Fatalf("retry delay = %s, want %s", wait.delay, preloadRetryDelay(attempt))
			}
			close(wait.release)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for retry %d", attempt)
		}
	}

	select {
	case call := <-hlsMaker.called:
		if call != 3 {
			t.Fatalf("HLS call = %d, want 3", call)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for successful HLS retry")
	}

	waitForPlaybackState(t, state, func(state *State) bool {
		return state.followingPrepared != nil && state.followingPrepared.songID == 3 && !state.preloadInFlight
	})
	if hlsMaker.Calls() != 3 {
		t.Fatalf("HLS calls = %d, want 3", hlsMaker.Calls())
	}
	if client.SongURLCalls(3) != 3 {
		t.Fatalf("song URL calls = %d, want 3 fresh URLs", client.SongURLCalls(3))
	}
	urls := hlsMaker.URLs()
	for i, trackURL := range urls {
		wantAttempt := "attempt=" + strconv.Itoa(i+1)
		if !strings.Contains(trackURL, wantAttempt) {
			t.Fatalf("HLS URL %d = %q, want fresh URL containing %q", i+1, trackURL, wantAttempt)
		}
	}
}

func TestState_MissingNextAtBoundaryKeepsPlayingUntilPrepared(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := newStateStationStore()
	client := &stateNetEaseClient{playlist: stateNetEasePlaylist()}
	netEaseService := netease.NewService(store, client, log)
	if err := netEaseService.Load(); err != nil {
		t.Fatalf("load netease service: %v", err)
	}

	hlsMaker := &flakyStateHLSMaker{failures: 1, called: make(chan int, 4)}
	state := NewStateWithHLSMaker(netEaseService, hlsMaker, t.TempDir(), log)
	state.NewTrackNotify = make(chan string, 1)
	state.PauseNotify = make(chan bool, 1)
	current := stateTrack(1, "One", "Artist A", "current-seg-", 10)
	state.CurrentTrack = current.track
	state.CurrentNetEaseID = current.songID
	state.CurrentTrackElapsed = current.track.Duration - state.refreshInterval
	state.IsPlaying = true
	state.playlist = hls.NewPlaylist(current.segments, nil)
	state.PlaylistStr = state.playlist.Generate(state.CurrentTrackElapsed)
	playlistBefore := state.PlaylistStr

	retryWaits := make(chan stateRetryWait, 1)
	state.preloadRetryWait = func(delay time.Duration) {
		wait := stateRetryWait{delay: delay, release: make(chan struct{})}
		retryWaits <- wait
		<-wait.release
	}
	state.ensurePreloaded()
	select {
	case call := <-hlsMaker.called:
		if call != 1 {
			t.Fatalf("HLS call = %d, want initial failed call 1", call)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for initial preload failure")
	}
	var retryWait stateRetryWait
	select {
	case retryWait = <-retryWaits:
		if retryWait.delay != preloadRetryInitialDelay {
			t.Fatalf("retry delay = %s, want %s", retryWait.delay, preloadRetryInitialDelay)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for preload retry")
	}

	state.refresh()

	state.mutex.Lock()
	isPlaying := state.IsPlaying
	currentSongID := state.CurrentNetEaseID
	currentTrack := state.CurrentTrack
	currentElapsed := state.CurrentTrackElapsed
	currentPlaylist := state.PlaylistStr
	state.mutex.Unlock()
	if !isPlaying {
		t.Fatal("playback paused while waiting for next track")
	}
	if currentSongID != current.songID || currentTrack == nil {
		t.Fatalf("current track changed while waiting: id=%d track=%#v", currentSongID, currentTrack)
	}
	if currentElapsed != current.track.Duration {
		t.Fatalf("elapsed = %f, want clamped duration %f", currentElapsed, current.track.Duration)
	}
	if currentPlaylist != playlistBefore {
		t.Fatal("playlist was cleared or replaced while waiting for next track")
	}
	select {
	case <-state.PauseNotify:
		t.Fatal("pause notification sent while waiting for next track")
	default:
	}

	close(retryWait.release)
	select {
	case call := <-hlsMaker.called:
		if call != 2 {
			t.Fatalf("HLS call = %d, want successful retry call 2", call)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for successful preload retry")
	}
	var preparedSongID int64
	waitForPlaybackState(t, state, func(state *State) bool {
		if state.nextPrepared == nil {
			return false
		}
		preparedSongID = state.nextPrepared.songID
		return true
	})

	state.refresh()
	snapshot := state.Snapshot()
	if !snapshot.IsPlaying {
		t.Fatal("playback paused after next track became ready")
	}
	if snapshot.CurrentNetEaseID != preparedSongID {
		t.Fatalf("current song = %d, want prepared song %d", snapshot.CurrentNetEaseID, preparedSongID)
	}
	if snapshot.CurrentTrackElapsed != 0 {
		t.Fatalf("elapsed = %f, want 0 after track transition", snapshot.CurrentTrackElapsed)
	}
	if !strings.Contains(state.Playlist(), snapshot.CurrentTrack.ID) {
		t.Fatalf("playlist does not contain current track %q", snapshot.CurrentTrack.ID)
	}
	select {
	case trackName := <-state.NewTrackNotify:
		if trackName != snapshot.CurrentTrack.DisplayName() {
			t.Fatalf("new track notification = %q, want %q", trackName, snapshot.CurrentTrack.DisplayName())
		}
	default:
		t.Fatal("new track notification was not sent")
	}
	select {
	case <-state.PauseNotify:
		t.Fatal("pause notification sent after retry recovery")
	default:
	}

	waitForPlaybackState(t, state, func(state *State) bool { return !state.preloadInFlight })
}

func TestState_StalePreloadDoesNotClearCurrentGeneration(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := newStateStationStore()
	client := &stateNetEaseClient{playlist: stateNetEasePlaylist()}
	netEaseService := netease.NewService(store, client, log)
	if err := netEaseService.Load(); err != nil {
		t.Fatalf("load netease service: %v", err)
	}

	oldRelease := make(chan struct{})
	newRelease := make(chan struct{})
	oldReleased := false
	newReleased := false
	t.Cleanup(func() {
		if !oldReleased {
			close(oldRelease)
		}
		if !newReleased {
			close(newRelease)
		}
	})
	hlsMaker := &controlledStateHLSMaker{
		called:   make(chan int, 2),
		releases: []<-chan struct{}{oldRelease, newRelease},
	}
	state := NewStateWithHLSMaker(netEaseService, hlsMaker, t.TempDir(), log)
	current := stateTrack(1, "One", "Artist A", "current-seg-", 10)
	next := stateTrack(2, "Two", "Artist B", "next-seg-", 10)
	state.CurrentTrack = current.track
	state.CurrentNetEaseID = current.songID
	state.nextPrepared = next
	state.IsPlaying = true
	state.playlist = hls.NewPlaylist(current.segments, next.segments)
	state.PlaylistStr = state.playlist.Generate(0)
	state.preloadInFlight = true
	oldGeneration := state.preloadGeneration

	oldDone := make(chan struct{})
	go func() {
		state.preload(oldGeneration)
		close(oldDone)
	}()
	select {
	case call := <-hlsMaker.called:
		if call != 1 {
			t.Fatalf("old-generation HLS call = %d, want 1", call)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for old-generation preload")
	}

	state.mutex.Lock()
	state.pauseLocked()
	state.CurrentTrack = current.track
	state.CurrentNetEaseID = current.songID
	state.nextPrepared = next
	state.IsPlaying = true
	state.playlist = hls.NewPlaylist(current.segments, next.segments)
	state.PlaylistStr = state.playlist.Generate(0)
	state.mutex.Unlock()
	state.ensurePreloaded()
	select {
	case call := <-hlsMaker.called:
		if call != 2 {
			t.Fatalf("current-generation HLS call = %d, want 2", call)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for current-generation preload")
	}

	close(oldRelease)
	oldReleased = true
	select {
	case <-oldDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stale preload to finish")
	}

	state.mutex.Lock()
	preloadInFlight := state.preloadInFlight
	followingPrepared := state.followingPrepared
	state.mutex.Unlock()
	if !preloadInFlight {
		t.Fatal("stale preload cleared the current generation's in-flight flag")
	}
	if followingPrepared != nil {
		t.Fatalf("stale preload installed a track in the current generation: %#v", followingPrepared)
	}

	close(newRelease)
	newReleased = true
	waitForPlaybackState(t, state, func(state *State) bool {
		return !state.preloadInFlight && state.followingPrepared != nil && state.followingPrepared.songID == 3
	})
}

func stateNetEasePlaylist() *netease.Playlist {
	return &netease.Playlist{
		ID:   "1",
		Name: "Playlist",
		Tracks: []*netease.Song{
			{ID: 1, Name: "One", Artists: []string{"Artist A"}, Duration: 10},
			{ID: 2, Name: "Two", Artists: []string{"Artist B"}, Duration: 10},
			{ID: 3, Name: "Three", Artists: []string{"Artist C"}, Duration: 10},
		},
	}
}

func waitForPlaybackState(t *testing.T, state *State, condition func(*State) bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		state.mutex.Lock()
		done := condition(state)
		state.mutex.Unlock()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for playback state")
		}
		time.Sleep(time.Millisecond)
	}
}

func stateTrack(songID int64, name, artist, segmentPrefix string, duration float64) *preparedTrack {
	song := &netease.Song{ID: songID, Name: name, Artists: []string{artist}, Duration: duration}
	return &preparedTrack{
		track:    song.Track(128),
		songID:   songID,
		url:      "https://example.test/" + strconv.FormatInt(songID, 10) + ".mp3",
		segments: stateSegments(segmentPrefix, duration),
		m3u8Path: "tmp/" + segmentPrefix + "all.m3u8",
	}
}

func stateSegments(prefix string, duration float64) []*hls.Segment {
	return []*hls.Segment{
		{Duration: duration / 2, Path: prefix + "0" + hls.SegmentExtension, IsFirst: true},
		{Duration: duration / 2, Path: prefix + "1" + hls.SegmentExtension},
	}
}

func recentNetEaseSongIDs(store *stateStationStore) []int64 {
	raw := store.props["netease_recent_song_ids"]
	if raw == "" {
		return nil
	}

	trimmed := strings.Trim(raw, "[]")
	if trimmed == "" {
		return nil
	}

	parts := strings.Split(trimmed, ",")
	ids := make([]int64, 0, len(parts))
	for _, part := range parts {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func TestState_TelegramSourcePathsFollowPlayback(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := newStateStationStore()
	client := &stateNetEaseClient{playlist: stateNetEasePlaylist()}
	netEaseService := netease.NewService(store, client, log)
	if err := netEaseService.Load(); err != nil {
		t.Fatalf("load netease service: %v", err)
	}

	state := NewStateWithHLSMaker(netEaseService, &stateHLSMaker{}, t.TempDir(), log)
	state.NewTrackNotify = make(chan string, 1)
	state.PlayNotify = make(chan string, 1)
	state.PauseNotify = make(chan bool, 1)
	if err := state.Play(); err != nil {
		t.Fatalf("start playback: %v", err)
	}

	current := state.CurrentHLSPlaylistPath()
	next := state.NextHLSPlaylistPath()
	if current == "" {
		t.Fatal("current HLS playlist path is empty while playing")
	}
	if next == "" {
		t.Fatal("next HLS playlist path is empty while playing")
	}
	if current == next {
		t.Fatalf("current and next HLS playlist paths are identical: %q", current)
	}

	state.Pause()
	if got := state.CurrentHLSPlaylistPath(); got != "" {
		t.Fatalf("current HLS playlist path not cleared on pause: %q", got)
	}
	if got := state.NextHLSPlaylistPath(); got != "" {
		t.Fatalf("next HLS playlist path not cleared on pause: %q", got)
	}
}

func TestState_CurrentHLSPlaylistPathAdvancesAtBoundary(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := newStateStationStore()
	client := &stateNetEaseClient{playlist: stateNetEasePlaylist()}
	netEaseService := netease.NewService(store, client, log)
	if err := netEaseService.Load(); err != nil {
		t.Fatalf("load netease service: %v", err)
	}

	state := NewStateWithHLSMaker(netEaseService, &stateHLSMaker{}, t.TempDir(), log)
	state.NewTrackNotify = make(chan string, 1)
	state.PlayNotify = make(chan string, 1)
	state.PauseNotify = make(chan bool, 1)
	if err := state.Play(); err != nil {
		t.Fatalf("start playback: %v", err)
	}

	nextBefore := state.NextHLSPlaylistPath()
	if nextBefore == "" {
		t.Fatal("next HLS playlist path is empty before the boundary")
	}

	state.mutex.Lock()
	state.CurrentTrackElapsed = state.CurrentTrack.Duration - state.refreshInterval
	state.mutex.Unlock()
	state.refresh()

	if got := state.CurrentHLSPlaylistPath(); got != nextBefore {
		t.Fatalf("current HLS playlist path = %q after boundary, want the former next path %q", got, nextBefore)
	}
}
