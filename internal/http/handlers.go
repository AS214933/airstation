package http

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"time"

	"github.com/cheatsnake/airstation/internal/netease"
	"github.com/cheatsnake/airstation/internal/pkg/sse"
	"github.com/cheatsnake/airstation/internal/station"
	"github.com/cheatsnake/airstation/internal/telegram"
	"github.com/golang-jwt/jwt/v5"
)

func (s *Server) handleHLSPlaylist(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "audio/mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")

	fmt.Fprint(w, s.playbackState.Playlist())
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	eventChan := make(chan *sse.Event)
	s.eventsEmitter.Subscribe(eventChan)

	closeNotify := r.Context().Done()
	go func() {
		<-closeNotify
		s.eventsEmitter.Unsubscribe(eventChan)
		close(eventChan)
	}()

	// Send current number of listeners immediately
	countEvent := s.countListeners()
	fmt.Fprint(w, countEvent.Stringify())
	w.(http.Flusher).Flush()

	for {
		event, isOpen := <-eventChan
		if !isOpen {
			break
		}

		fmt.Fprint(w, event.Stringify())
		w.(http.Flusher).Flush()
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	body, err := parseJSONBody[struct {
		Secret string `json:"secret"`
	}](r)
	if err != nil {
		jsonBadRequest(w, "Parsing request body failed.")
		return
	}

	isValidSecret := subtle.ConstantTimeCompare([]byte(body.Secret), []byte(s.config.SecretKey)) == 1
	if !isValidSecret {
		jsonForbidden(w, "Wrong secret, access denied.")
		return
	}

	expirationTime := time.Now().Add(7 * 24 * time.Hour)
	claims := jwt.MapClaims{
		"iss": "airstation",
		"exp": expirationTime.Unix(),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte(s.config.JWTSign))
	if err != nil {
		s.logger.Debug("Failed to generate token: " + err.Error())
		jsonInternalError(w, "Failed to generate token.")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "jwt",
		Value:    tokenString,
		Expires:  expirationTime,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.config.SecureCookie,
		SameSite: http.SameSiteStrictMode,
	})

	s.logger.Info(fmt.Sprintf("New login succeed from %s with secureCookie=%v", r.Host, s.config.SecureCookie))

	jsonOK(w, "Login succeed.")
}

func (s *Server) handlePlaybackState(w http.ResponseWriter, _ *http.Request) {
	jsonResponse(w, s.playbackState.Snapshot())
}

func (s *Server) handlePausePlayback(w http.ResponseWriter, _ *http.Request) {
	s.playbackState.Pause()
	jsonResponse(w, s.playbackState.Snapshot())
}

func (s *Server) handlePlayPlayback(w http.ResponseWriter, _ *http.Request) {
	err := s.playbackState.Play()
	if err != nil {
		jsonBadRequest(w, "Playback failed to start: "+err.Error())
		return
	}

	jsonResponse(w, s.playbackState.Snapshot())
}

func (s *Server) handlePlaybackLyrics(w http.ResponseWriter, _ *http.Request) {
	lyrics, err := s.playbackState.Lyrics()
	if err != nil {
		jsonResponse(w, &netease.Lyrics{Kind: "none"})
		return
	}

	jsonResponse(w, lyrics)
}

func (s *Server) handleNetEaseConfig(w http.ResponseWriter, _ *http.Request) {
	jsonResponse(w, s.netEaseService.Config())
}

func (s *Server) handleEditNetEaseConfig(w http.ResponseWriter, r *http.Request) {
	body, err := parseJSONBody[netease.Config](r)
	if err != nil {
		jsonBadRequest(w, "Parsing request body failed: "+err.Error())
		return
	}

	config, err := s.netEaseService.EditConfig(*body)
	if err != nil {
		jsonBadRequest(w, "NetEase config update failed: "+err.Error())
		return
	}

	if s.playbackState.Snapshot().IsPlaying {
		if err := s.playbackState.Reload(); err != nil {
			s.logger.Debug("Playback reload failed: " + err.Error())
		}
	}

	jsonResponse(w, config)
}

func (s *Server) handleSyncNetEasePlaylist(w http.ResponseWriter, _ *http.Request) {
	if err := s.netEaseService.Sync(); err != nil {
		jsonBadRequest(w, "NetEase playlist sync failed: "+err.Error())
		return
	}

	jsonResponse(w, s.netEaseService.Config())
}

func (s *Server) handleStaticDir(prefix string, path string) http.Handler {
	return http.StripPrefix(prefix, http.FileServer(http.Dir(path)))
}

func (s *Server) handleStaticDirWithSegmentCache(prefix string, path string) http.Handler {
	fileHandler := http.StripPrefix(prefix, http.FileServer(http.Dir(path)))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600, immutable")
		fileHandler.ServeHTTP(w, r)
	})
}

func (s *Server) handleStationInfo(w http.ResponseWriter, _ *http.Request) {
	info, err := s.stationService.Info()
	if err != nil {
		jsonBadRequest(w, "Failed to get station info: "+err.Error())
		return
	}

	jsonResponse(w, info)
}

func (s *Server) handleEditStationInfo(w http.ResponseWriter, r *http.Request) {
	body, err := parseJSONBody[station.Info](r)
	if err != nil {
		jsonBadRequest(w, "Parsing request body failed: "+err.Error())
		return
	}

	info, err := s.stationService.EditInfo(body)
	if err != nil {
		jsonBadRequest(w, "Station info editing failed: "+err.Error())
		return
	}

	s.eventsEmitter.RegisterEvent(eventChangeTheme, " ")

	jsonResponse(w, info)
}

func (s *Server) handleTelegramConfig(w http.ResponseWriter, _ *http.Request) {
	jsonResponse(w, s.telegramService.PublicConfig())
}

func (s *Server) handleEditTelegramConfig(w http.ResponseWriter, r *http.Request) {
	body, err := parseJSONBody[telegram.Config](r)
	if err != nil {
		jsonBadRequest(w, "Parsing request body failed: "+err.Error())
		return
	}

	config, err := s.telegramService.EditConfig(*body)
	if err != nil {
		jsonBadRequest(w, "Telegram voice stream config update failed: "+err.Error())
		return
	}

	jsonResponse(w, config)
}

func (s *Server) handleTestTelegramConfig(w http.ResponseWriter, r *http.Request) {
	body, err := parseJSONBody[telegram.Config](r)
	if err != nil {
		jsonBadRequest(w, "Parsing request body failed: "+err.Error())
		return
	}

	if err := s.telegramService.Test(*body); err != nil {
		jsonBadRequest(w, "Telegram credentials test failed: "+err.Error())
		return
	}

	jsonOK(w, "Telegram credentials are valid.")
}
