package http

import (
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/cheatsnake/airstation/internal/config"
	"github.com/cheatsnake/airstation/internal/netease"
	"github.com/cheatsnake/airstation/internal/pkg/ffmpeg"
	"github.com/cheatsnake/airstation/internal/pkg/hls"
	"github.com/cheatsnake/airstation/internal/pkg/sse"
	"github.com/cheatsnake/airstation/internal/playback"
	"github.com/cheatsnake/airstation/internal/station"
	"github.com/cheatsnake/airstation/internal/storage"
	"github.com/cheatsnake/airstation/internal/telegram"
	"github.com/rs/cors"
)

type Server struct {
	playbackState   *playback.State
	eventsEmitter   *sse.Emitter
	stationService  *station.Service
	netEaseService  *netease.Service
	telegramService *telegram.Service
	config          *config.Config
	logger          *slog.Logger
	router          *http.ServeMux
}

const telegramHLSPath = "/stream.m3u8"

func telegramHLSURL(port string) string {
	return "http://127.0.0.1:" + port + telegramHLSPath
}

func NewServer(store storage.Storage, conf *config.Config, logger *slog.Logger) *Server {
	ffmpegCLI := ffmpeg.NewCLI()
	ss := station.NewService(store)
	netEaseClient := netease.NewHTTPClient(netease.WithRealIP(conf.NetEaseRealIP))
	ns := netease.NewService(store, netEaseClient, logger.WithGroup("netease"))
	state := playback.NewState(ns, ffmpegCLI, conf.TmpDir, logger.WithGroup("playback"))
	ts := telegram.NewService(store, logger.WithGroup("telegram"))
	ts.SetHLSURL(telegramHLSURL(conf.HTTPPort))

	return &Server{
		playbackState:   state,
		eventsEmitter:   sse.NewEmitter(),
		stationService:  ss,
		netEaseService:  ns,
		telegramService: ts,
		config:          conf,
		logger:          logger.WithGroup("http"),
		router:          http.NewServeMux(),
	}
}

func (s *Server) Run() {
	s.registerStreamingMimeTypes()

	// Public handlers
	s.router.HandleFunc("GET /stream", s.handleHLSPlaylist)
	s.router.HandleFunc("GET "+telegramHLSPath, s.handleHLSPlaylist)
	s.router.HandleFunc("GET /api/v1/events", s.handleEvents)
	s.router.HandleFunc("GET /api/v1/station/info", s.handleStationInfo)
	s.router.HandleFunc("POST /api/v1/login", s.handleLogin)
	s.router.Handle("GET /static/tmp/", s.handleStaticDirWithSegmentCache("/static/tmp", s.config.TmpDir))
	s.router.Handle("GET /api/v1/playback", http.HandlerFunc(s.handlePlaybackState))
	s.router.Handle("GET /api/v1/playback/lyrics", http.HandlerFunc(s.handlePlaybackLyrics))

	// Protected handlers
	s.router.Handle("POST /api/v1/playback/pause", s.jwtAuth(http.HandlerFunc(s.handlePausePlayback)))
	s.router.Handle("POST /api/v1/playback/play", s.jwtAuth(http.HandlerFunc(s.handlePlayPlayback)))
	s.router.Handle("GET /api/v1/netease/config", s.jwtAuth(http.HandlerFunc(s.handleNetEaseConfig)))
	s.router.Handle("PUT /api/v1/netease/config", s.jwtAuth(http.HandlerFunc(s.handleEditNetEaseConfig)))
	s.router.Handle("POST /api/v1/netease/sync", s.jwtAuth(http.HandlerFunc(s.handleSyncNetEasePlaylist)))
	s.router.Handle("PUT /api/v1/station/info", s.jwtAuth(http.HandlerFunc(s.handleEditStationInfo)))
	s.router.Handle("GET /api/v1/telegram/config", s.jwtAuth(http.HandlerFunc(s.handleTelegramConfig)))
	s.router.Handle("PUT /api/v1/telegram/config", s.jwtAuth(http.HandlerFunc(s.handleEditTelegramConfig)))
	s.router.Handle("POST /api/v1/telegram/test", s.jwtAuth(http.HandlerFunc(s.handleTestTelegramConfig)))
	s.router.Handle("POST /api/v1/telegram/login/code", s.jwtAuth(http.HandlerFunc(s.handleTelegramLoginCode)))
	s.router.Handle("POST /api/v1/telegram/login/signin", s.jwtAuth(http.HandlerFunc(s.handleTelegramLoginSignIn)))
	s.router.Handle("DELETE /api/v1/telegram/session", s.jwtAuth(http.HandlerFunc(s.handleTelegramLogout)))

	s.router.Handle("GET /studio/", s.handleStaticDir("/studio/", s.config.StudioDir))
	s.router.Handle("GET /", s.handleStaticDir("/", s.config.PlayerDir))

	s.listenEvents()

	if err := s.telegramService.Load(); err != nil {
		s.logger.Warn("Telegram config loading failed: " + err.Error())
	}
	if err := s.netEaseService.Load(); err != nil {
		s.logger.Warn("NetEase config loading failed: " + err.Error())
	}

	err := s.playbackState.Play()
	if err != nil {
		s.logger.Warn("Auto start playing failed: " + err.Error())
	}

	// Only start the Telegram voice streamer when playback is actually active;
	// otherwise it would reconnect to an empty HLS playlist indefinitely.
	if s.playbackState.Snapshot().IsPlaying {
		if err := s.telegramService.Start(); err != nil {
			s.logger.Warn("Telegram streamer start failed: " + err.Error())
		}
	}

	go s.playbackState.Run()
	go s.netEaseService.RunAutoSync(nil)

	s.logger.Info("Server starts on http://localhost:" + s.config.HTTPPort)
	err = http.ListenAndServe(":"+s.config.HTTPPort, cors.Default().Handler(s.router))
	if err != nil {
		s.logger.Error("Listen and serve failed", slog.String("info", err.Error()))
	}
}

func (s *Server) registerStreamingMimeTypes() {
	if err := mime.AddExtensionType(hls.SegmentExtension, "video/iso.segment"); err != nil {
		s.logger.Error("HLS segment mime type registration failed", slog.String("info", err.Error()))
	}
	if err := mime.AddExtensionType(hls.InitSegmentExtension, "audio/mp4"); err != nil {
		s.logger.Error("HLS init segment mime type registration failed", slog.String("info", err.Error()))
	}
}

func (s *Server) countListeners() *sse.Event {
	count := s.eventsEmitter.CountSubscribers()
	return sse.NewEvent(eventCountListeners, strconv.Itoa(count))
}

func (s *Server) listenEvents() {
	countConnectionTicker := time.Tick(5 * time.Second)

	// TODO: add context for gracefull shutdown

	go func() {
		for range countConnectionTicker {
			event := s.countListeners()
			s.eventsEmitter.RegisterEvent(event.Name, event.Data)
		}
	}()

	go func() {
		for {
			select {
			case trackName := <-s.playbackState.PlayNotify:
				s.eventsEmitter.RegisterEvent(eventPlay, trackName)
				go s.syncTelegramStreamer()
			case <-s.playbackState.PauseNotify:
				s.eventsEmitter.RegisterEvent(eventPause, " ")
				go s.syncTelegramStreamer()
			case trackName := <-s.playbackState.NewTrackNotify:
				s.eventsEmitter.RegisterEvent(eventNewTrack, trackName)
				go s.syncTelegramStreamer()
			}
		}
	}()
}

// syncTelegramStreamer starts or stops the Telegram voice streamer based on
// the current playback state, so it only consumes the HLS playlist when audio
// is actually available.
func (s *Server) syncTelegramStreamer() {
	if s.playbackState.Snapshot().IsPlaying {
		if err := s.telegramService.Start(); err != nil {
			s.logger.Warn("Telegram streamer start failed: " + err.Error())
		}
	} else {
		if err := s.telegramService.Stop(); err != nil {
			s.logger.Warn("Telegram streamer stop failed: " + err.Error())
		}
	}
}

// Shutdown stops the Telegram streamer subprocess and any background goroutines.
func (s *Server) Shutdown() error {
	return s.telegramService.Stop()
}
