package http

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/cheatsnake/airstation/internal/netease"
	"github.com/cheatsnake/airstation/internal/pkg/ffmpeg"
	"github.com/cheatsnake/airstation/internal/pkg/sse"
	"github.com/cheatsnake/airstation/internal/station"
	"github.com/cheatsnake/airstation/internal/telegram"
	"github.com/golang-jwt/jwt/v5"
)

func (s *Server) handleHLSPlaylist(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "audio/mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")

	pl := s.playbackState.Playlist()
	if pl == "" {
		s.logger.Warn("HLS playlist is empty", slog.String("url", r.URL.String()), slog.Bool("isPlaying", s.playbackState.Snapshot().IsPlaying))
	} else {
		s.logger.Debug("HLS playlist served", slog.String("url", r.URL.String()), slog.Int("length", len(pl)))
	}

	fmt.Fprint(w, pl)
}

// handleTelegramStream serves one continuous ADTS audio stream consumed by the
// Telegram voice streamer. Each track is remuxed from its finished on-disk HLS
// playlist, so ffmpeg never has to follow a growing playlist: the streamer
// chains the current track into the next prepared track with no gap at track
// boundaries.
func (s *Server) handleTelegramStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	out := &flushWriter{w: w, flusher: flusher}

	ctx := r.Context()
	lastPlayed := ""
	for {
		if ctx.Err() != nil {
			return
		}
		path := s.nextTelegramSourcePath(lastPlayed)
		if path == "" {
			time.Sleep(200 * time.Millisecond)
			continue
		}

		streamCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			done <- ffmpeg.StreamHLSAsADTS(streamCtx, path, out)
		}()

		lastPlayed = path
		s.logger.Info("streaming Telegram audio source", slog.String("path", path))

	streaming:
		for {
			select {
			case err := <-done:
				cancel()
				if err != nil && ctx.Err() == nil {
					s.logger.Warn("Telegram ADTS remux failed", slog.String("error", err.Error()))
					time.Sleep(500 * time.Millisecond)
				}
				break streaming
			case <-time.After(200 * time.Millisecond):
				// Stop early when the track is no longer part of the queue
				// (pause or a track switch the web player already made).
				if !s.telegramTrackStillActive(path) {
					cancel()
					<-done
					break streaming
				}
			}
		}
	}
}

// telegramTrackStillActive reports whether path is still part of the stream
// queue: it must be either the current track or the next prepared track. The
// streamer keeps playing the current track even when the next one is not
// prepared yet; it only stops early when the web player has already switched
// past it or playback paused.
func (s *Server) telegramTrackStillActive(path string) bool {
	if s.playbackState.CurrentHLSPlaylistPath() == path {
		return true
	}
	return s.playbackState.NextHLSPlaylistPath() == path
}

// nextTelegramSourcePath returns the next m3u8 path the Telegram streamer
// should play after lastPlayed: the current track when it has not been played
// yet, otherwise the next prepared track, so the stream chains tracks without
// waiting for the playback state to switch.
func (s *Server) nextTelegramSourcePath(lastPlayed string) string {
	current := s.playbackState.CurrentHLSPlaylistPath()
	if current != "" && current != lastPlayed {
		return current
	}
	next := s.playbackState.NextHLSPlaylistPath()
	if next != "" && next != lastPlayed {
		return next
	}
	return ""
}

// flushWriter flushes the underlying ResponseWriter after every write so the
// ADTS bytes reach the Telegram streamer's ffmpeg as soon as they are ready.
type flushWriter struct {
	w       io.Writer
	flusher http.Flusher
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if fw.flusher != nil {
		fw.flusher.Flush()
	}
	return n, err
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

func (s *Server) handleTelegramLoginCode(w http.ResponseWriter, r *http.Request) {
	body, err := parseJSONBody[struct {
		Phone   string             `json:"phone"`
		APIID   telegram.IntString `json:"apiID"`
		APIHash string             `json:"apiHash"`
	}](r)
	if err != nil {
		jsonBadRequest(w, "Parsing request body failed: "+err.Error())
		return
	}

	phoneCodeHash, err := s.telegramService.SendLoginCode(body.Phone, body.APIID.Int(), body.APIHash)
	if err != nil {
		jsonBadRequest(w, "Telegram login code request failed: "+err.Error())
		return
	}

	jsonResponse(w, telegram.LoginCodeResponse{PhoneCodeHash: phoneCodeHash})
}

func (s *Server) handleTelegramLoginSignIn(w http.ResponseWriter, r *http.Request) {
	body, err := parseJSONBody[struct {
		Phone         string             `json:"phone"`
		PhoneCodeHash string             `json:"phoneCodeHash"`
		Code          string             `json:"code"`
		Password      string             `json:"password"`
		APIID         telegram.IntString `json:"apiID"`
		APIHash       string             `json:"apiHash"`
	}](r)
	if err != nil {
		jsonBadRequest(w, "Parsing request body failed: "+err.Error())
		return
	}

	err = s.telegramService.SignInUserbot(body.Phone, body.PhoneCodeHash, body.Code, body.Password, body.APIID.Int(), body.APIHash)
	if errors.Is(err, telegram.ErrPasswordNeeded) {
		jsonResponse(w, struct {
			NeedsPassword bool `json:"needsPassword"`
		}{NeedsPassword: true})
		return
	}
	if err != nil {
		jsonBadRequest(w, "Telegram sign-in failed: "+err.Error())
		return
	}

	jsonOK(w, "Telegram userbot logged in and session saved.")
}

func (s *Server) handleTelegramLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.telegramService.ClearSession(); err != nil {
		jsonBadRequest(w, "Failed to clear Telegram session: "+err.Error())
		return
	}

	jsonOK(w, "Telegram session cleared.")
}
