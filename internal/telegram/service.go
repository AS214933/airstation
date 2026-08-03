package telegram

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-faster/errors"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/calls"
	"github.com/gotd/td/tg"
	"github.com/pion/rtp"
)

// ErrPasswordNeeded is returned by SignInUserbot when the account requires
// a 2FA password to complete sign-in.
var ErrPasswordNeeded = errors.New("2FA password required")

// Service manages Telegram voice stream configuration and the in-process gotd
// voice streamer.
type Service struct {
	store  Store
	logger *slog.Logger

	mutex        sync.RWMutex
	config       Config
	streamCtx    context.Context
	streamCancel context.CancelFunc
	streamWg     sync.WaitGroup
}

// NewService creates a new Telegram voice stream service.
func NewService(store Store, log *slog.Logger) *Service {
	return &Service{
		store:  store,
		logger: log.WithGroup("telegram"),
	}
}

// localStreamURL returns the URL of the local Airstation HLS playlist. The
// Telegram voice streamer always consumes this URL so it does not require
// external network access. It respects the AIRSTATION_HTTP_PORT environment
// variable, defaulting to 7331.
func localStreamURL() string {
	port := os.Getenv("AIRSTATION_HTTP_PORT")
	if port == "" {
		port = "7331"
	}
	return "http://localhost:" + port + "/stream"
}

// Load reads Telegram voice stream configuration from persistent storage.
func (s *Service) Load() error {
	props, err := s.store.StationProperties()
	if err != nil {
		return err
	}

	cfg := Config{}
	for _, prop := range props {
		switch prop.Key {
		case propEnabled:
			cfg.Enabled = strings.EqualFold(prop.Value, "true") || prop.Value == "1"
		case propAPIID:
			if v, err := strconv.Atoi(prop.Value); err == nil {
				cfg.APIID = IntString(v)
			}
		case propAPIHash:
			cfg.APIHash = prop.Value
		case propSessionString:
			cfg.SessionString = prop.Value
		case propStreamURL:
			cfg.StreamURL = prop.Value
		case propChatIDs:
			cfg.ChatIDs = parseChatIDs(prop.Value)
		}
	}

	s.mutex.Lock()
	s.config = cfg
	s.mutex.Unlock()

	return nil
}

func parseChatIDs(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	ids := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			ids = append(ids, p)
		}
	}
	return ids
}

func formatChatIDs(ids []string) string {
	return strings.Join(ids, ",")
}

// Config returns the current full configuration (including secrets) under lock.
func (s *Service) Config() Config {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return s.config
}

// PublicConfig returns a sanitized configuration for public API responses.
// Secrets are never returned.
func (s *Service) PublicConfig() PublicConfig {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	return PublicConfig{
		Enabled:    s.config.Enabled,
		StreamURL:  s.config.StreamURL,
		ChatIDs:    append([]string(nil), s.config.ChatIDs...),
		HasAPIID:   s.config.APIID != 0,
		HasAPIHash: s.config.APIHash != "",
		HasSession: s.config.SessionString != "",
	}
}

// EditConfig validates and saves a new configuration, then restarts the streamer
// so changes take effect immediately. Empty secret fields are merged with the
// currently stored values so callers can send "" for unchanged secrets.
func (s *Service) EditConfig(newConfig Config) (PublicConfig, error) {
	// Merge empty secrets with existing stored values.
	s.mutex.Lock()
	if newConfig.APIID == 0 {
		newConfig.APIID = s.config.APIID
	}
	if strings.TrimSpace(newConfig.APIHash) == "" {
		newConfig.APIHash = s.config.APIHash
	}
	if strings.TrimSpace(newConfig.SessionString) == "" {
		newConfig.SessionString = s.config.SessionString
	}
	s.mutex.Unlock()

	if newConfig.Enabled {
		if newConfig.APIID.Int() == 0 {
			return PublicConfig{}, errors.New("Telegram API ID is required")
		}
		if strings.TrimSpace(newConfig.APIHash) == "" {
			return PublicConfig{}, errors.New("Telegram API hash is required")
		}
		if strings.TrimSpace(newConfig.SessionString) == "" {
			return PublicConfig{}, errors.New("Telegram session is required; log in via the userbot login flow first")
		}
		if len(newConfig.ChatIDs) == 0 {
			return PublicConfig{}, errors.New("at least one Telegram chat ID is required")
		}
	}

	cleanedIDs := make([]string, 0, len(newConfig.ChatIDs))
	for _, id := range newConfig.ChatIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, err := strconv.ParseInt(id, 10, 64); err != nil {
			return PublicConfig{}, fmt.Errorf("invalid chat ID %q: %w", id, err)
		}
		cleanedIDs = append(cleanedIDs, id)
	}
	newConfig.ChatIDs = cleanedIDs
	// Stream URL is ignored; the streamer always uses the local Airstation HLS
	// playlist. Keep the field empty in storage for clarity.
	newConfig.StreamURL = ""

	if _, err := s.store.UpsertStationProperty(propEnabled, strconv.FormatBool(newConfig.Enabled)); err != nil {
		return PublicConfig{}, err
	}
	if _, err := s.store.UpsertStationProperty(propAPIID, strconv.Itoa(newConfig.APIID.Int())); err != nil {
		return PublicConfig{}, err
	}
	if _, err := s.store.UpsertStationProperty(propAPIHash, newConfig.APIHash); err != nil {
		return PublicConfig{}, err
	}
	if _, err := s.store.UpsertStationProperty(propSessionString, newConfig.SessionString); err != nil {
		return PublicConfig{}, err
	}
	if _, err := s.store.UpsertStationProperty(propStreamURL, newConfig.StreamURL); err != nil {
		return PublicConfig{}, err
	}
	if _, err := s.store.UpsertStationProperty(propChatIDs, formatChatIDs(newConfig.ChatIDs)); err != nil {
		return PublicConfig{}, err
	}

	s.mutex.Lock()
	s.config = newConfig
	s.mutex.Unlock()

	if newConfig.Enabled {
		if err := s.Restart(); err != nil {
			s.logger.Warn("Failed to restart Telegram streamer after config change", slog.String("error", err.Error()))
		}
	} else {
		if err := s.Stop(); err != nil {
			s.logger.Warn("Failed to stop Telegram streamer after disable", slog.String("error", err.Error()))
		}
	}

	return s.PublicConfig(), nil
}

// Test validates Telegram credentials by attempting a quick gotd connection.
func (s *Service) Test(cfg Config) error {
	s.mutex.RLock()
	if cfg.APIID == 0 {
		cfg.APIID = s.config.APIID
	}
	if strings.TrimSpace(cfg.APIHash) == "" {
		cfg.APIHash = s.config.APIHash
	}
	if strings.TrimSpace(cfg.SessionString) == "" {
		cfg.SessionString = s.config.SessionString
	}
	s.mutex.RUnlock()

	if cfg.APIID.Int() == 0 {
		return errors.New("Telegram API ID is required")
	}
	if strings.TrimSpace(cfg.APIHash) == "" {
		return errors.New("Telegram API hash is required")
	}
	if strings.TrimSpace(cfg.SessionString) == "" {
		return errors.New("Telegram session is required")
	}

	sessionStore := &persistedSessionStorage{s: s}
	client := telegram.NewClient(cfg.APIID.Int(), cfg.APIHash, telegram.Options{
		SessionStorage: sessionStore,
		UpdateHandler:  tg.NewUpdateDispatcher(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var testErr error
	err := client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			testErr = err
			return nil
		}
		if !status.Authorized {
			testErr = errors.New("Telegram session is not authorized")
			return nil
		}
		if status.User != nil && status.User.Bot {
			testErr = errors.New("bot accounts cannot be used for Telegram voice streaming; use a user account")
		}
		return nil
	})
	if testErr != nil {
		return testErr
	}
	return err
}

// resolveAPICredentials returns the provided API credentials when they are set,
// otherwise falling back to the values stored in the service configuration.
func (s *Service) resolveAPICredentials(apiID int, apiHash string) (int, string, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	if apiID == 0 {
		apiID = s.config.APIID.Int()
	}
	if apiHash == "" {
		apiHash = s.config.APIHash
	}
	if apiID == 0 {
		return 0, "", errors.New("Telegram API ID is required")
	}
	if apiHash == "" {
		return 0, "", errors.New("Telegram API hash is required")
	}
	return apiID, apiHash, nil
}

func (s *Service) loadLoginState() (phone, phoneCodeHash, step, sessionData string, err error) {
	props, err := s.store.StationProperties()
	if err != nil {
		return "", "", "", "", err
	}
	for _, p := range props {
		switch p.Key {
		case propLoginPhone:
			phone = p.Value
		case propLoginPhoneCodeHash:
			phoneCodeHash = p.Value
		case propLoginStep:
			step = p.Value
		case propLoginSessionData:
			sessionData = p.Value
		}
	}
	return phone, phoneCodeHash, step, sessionData, nil
}

func (s *Service) saveLoginState(phone, phoneCodeHash, step, sessionData string) error {
	if _, err := s.store.UpsertStationProperty(propLoginPhone, phone); err != nil {
		return err
	}
	if _, err := s.store.UpsertStationProperty(propLoginPhoneCodeHash, phoneCodeHash); err != nil {
		return err
	}
	if _, err := s.store.UpsertStationProperty(propLoginStep, step); err != nil {
		return err
	}
	if _, err := s.store.UpsertStationProperty(propLoginSessionData, sessionData); err != nil {
		return err
	}
	return nil
}

func (s *Service) clearLoginState() error {
	_ = s.store.DeleteStationProperty(propLoginPhone)
	_ = s.store.DeleteStationProperty(propLoginPhoneCodeHash)
	_ = s.store.DeleteStationProperty(propLoginStep)
	_ = s.store.DeleteStationProperty(propLoginSessionData)
	return nil
}

// SendLoginCode requests a Telegram login code for the given phone number.
// It returns the phone_code_hash needed for SignInUserbot and persists the
// login state so the 2FA step can continue later.
func (s *Service) SendLoginCode(phone string, apiID int, apiHash string) (string, error) {
	apiID, apiHash, err := s.resolveAPICredentials(apiID, apiHash)
	if err != nil {
		return "", err
	}
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return "", errors.New("phone number is required")
	}

	s.clearLoginState()

	sessionStore := &memorySessionStorage{}
	client := telegram.NewClient(apiID, apiHash, telegram.Options{
		SessionStorage: sessionStore,
		UpdateHandler:  tg.NewUpdateDispatcher(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var codeHash string
	var runErr error
	err = client.Run(ctx, func(ctx context.Context) error {
		sentCode, err := client.Auth().SendCode(ctx, phone, auth.SendCodeOptions{})
		if err != nil {
			runErr = err
			return nil
		}
		switch s := sentCode.(type) {
		case *tg.AuthSentCode:
			codeHash = s.PhoneCodeHash
		default:
			runErr = fmt.Errorf("unexpected sent code type %T", sentCode)
		}
		return nil
	})
	if runErr != nil {
		return "", runErr
	}
	if err != nil {
		return "", err
	}
	if codeHash == "" {
		return "", errors.New("login code response did not include phone_code_hash")
	}

	encodedSession := base64.StdEncoding.EncodeToString(sessionStore.bytes())
	if err := s.saveLoginState(phone, codeHash, "code_sent", encodedSession); err != nil {
		return "", err
	}

	return codeHash, nil
}

// SignInUserbot completes a userbot login with the verification code and optional
// 2FA password. If the account requires a password, it returns ErrPasswordNeeded
// and the caller must retry with the password. On success the resulting session
// data is persisted and the streamer is restarted so the new session takes
// effect immediately.
func (s *Service) SignInUserbot(phone, phoneCodeHash, code, password string, apiID int, apiHash string) error {
	apiID, apiHash, err := s.resolveAPICredentials(apiID, apiHash)
	if err != nil {
		return err
	}

	phone = strings.TrimSpace(phone)
	if phone == "" {
		return errors.New("phone number is required")
	}
	if strings.TrimSpace(phoneCodeHash) == "" {
		return errors.New("phone code hash is required")
	}
	if strings.TrimSpace(code) == "" {
		return errors.New("login code is required")
	}

	storedPhone, storedHash, step, encodedSession, err := s.loadLoginState()
	if err != nil {
		return err
	}
	awaitingPassword := step == "awaiting_password" && storedHash == phoneCodeHash && storedPhone == phone

	sessionData, err := base64.StdEncoding.DecodeString(encodedSession)
	if err != nil {
		return fmt.Errorf("decoding login session: %w", err)
	}

	sessionStore := &memorySessionStorage{data: sessionData}
	client := telegram.NewClient(apiID, apiHash, telegram.Options{
		SessionStorage: sessionStore,
		UpdateHandler:  tg.NewUpdateDispatcher(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var signInErr error
	err = client.Run(ctx, func(ctx context.Context) error {
		if awaitingPassword && password != "" {
			_, signInErr = client.Auth().Password(ctx, password)
			return nil
		}

		_, signInErr = client.Auth().SignIn(ctx, phone, code, phoneCodeHash)
		if errors.Is(signInErr, auth.ErrPasswordAuthNeeded) {
			if password == "" {
				signInErr = ErrPasswordNeeded
				return nil
			}
			_, signInErr = client.Auth().Password(ctx, password)
		}
		return nil
	})
	if signInErr != nil {
		if errors.Is(signInErr, ErrPasswordNeeded) {
			if err := s.saveLoginState(phone, phoneCodeHash, "awaiting_password", base64.StdEncoding.EncodeToString(sessionStore.bytes())); err != nil {
				return err
			}
			return ErrPasswordNeeded
		}
		return signInErr
	}
	if err != nil {
		return err
	}

	encoded := base64.StdEncoding.EncodeToString(sessionStore.bytes())
	if _, err := s.store.UpsertStationProperty(propSessionString, encoded); err != nil {
		return err
	}

	s.mutex.Lock()
	s.config.SessionString = encoded
	enabled := s.config.Enabled
	s.mutex.Unlock()

	if err := s.clearLoginState(); err != nil {
		return err
	}

	s.logger.Info("Telegram userbot session saved")

	if enabled {
		if err := s.Restart(); err != nil {
			s.logger.Warn("Failed to restart Telegram streamer after login", slog.String("error", err.Error()))
		}
	}

	return nil
}

// ClearSession removes the persisted session string and any pending login state,
// clears them from the in-memory config, and stops the streamer if it is running.
func (s *Service) ClearSession() error {
	if err := s.store.DeleteStationProperty(propSessionString); err != nil {
		return err
	}
	_ = s.clearLoginState()

	s.mutex.Lock()
	s.config.SessionString = ""
	s.mutex.Unlock()

	if err := s.Stop(); err != nil {
		s.logger.Warn("Failed to stop Telegram streamer after clearing session", slog.String("error", err.Error()))
	}
	return nil
}

// Start spawns the gotd voice streamer goroutine if enabled and not already running.
func (s *Service) Start() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if !s.config.Enabled {
		return nil
	}
	if s.config.APIID == 0 || s.config.APIHash == "" || s.config.SessionString == "" || len(s.config.ChatIDs) == 0 {
		return errors.New("Telegram voice stream is enabled but not fully configured")
	}
	if s.streamCtx != nil {
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.streamCtx = ctx
	s.streamCancel = cancel
	s.streamWg.Add(1)
	go s.streamLoop(ctx)

	s.logger.Info("Telegram voice streamer started")
	return nil
}

// Stop terminates the running streamer goroutine.
func (s *Service) Stop() error {
	s.mutex.Lock()
	cancel := s.streamCancel
	s.streamCancel = nil
	s.streamCtx = nil
	s.mutex.Unlock()

	if cancel != nil {
		cancel()
	}
	s.streamWg.Wait()
	s.logger.Info("Telegram voice streamer stopped")
	return nil
}

// Restart stops and then starts the streamer.
func (s *Service) Restart() error {
	if err := s.Stop(); err != nil {
		s.logger.Warn("Error stopping streamer during restart", slog.String("error", err.Error()))
	}
	return s.Start()
}

func (s *Service) streamLoop(ctx context.Context) {
	defer s.streamWg.Done()

	s.mutex.RLock()
	apiID := s.config.APIID.Int()
	apiHash := s.config.APIHash
	chatIDs := append([]string(nil), s.config.ChatIDs...)
	// Always stream the local Airstation HLS playlist; ignore any configured
	// external stream URL so no outbound network access is required.
	streamURL := localStreamURL()
	s.mutex.RUnlock()

	sessionStore := &persistedSessionStorage{s: s}
	dispatcher := tg.NewUpdateDispatcher()
	client := telegram.NewClient(apiID, apiHash, telegram.Options{
		SessionStorage: sessionStore,
		UpdateHandler:  dispatcher,
	})

	if err := client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("auth status: %w", err)
		}
		if !status.Authorized {
			return errors.New("Telegram session is not authorized")
		}
		self, err := client.Self(ctx)
		if err != nil {
			return fmt.Errorf("get self: %w", err)
		}
		joinAs := &tg.InputPeerUser{UserID: self.ID, AccessHash: self.AccessHash}

		peers, err := fetchInputPeers(ctx, client.API())
		if err != nil {
			s.logger.Warn("Failed to fetch input peers", slog.String("error", err.Error()))
		}

		var wg sync.WaitGroup
		for _, idStr := range chatIDs {
			id, err := strconv.ParseInt(idStr, 10, 64)
			if err != nil {
				s.logger.Warn("Skipping invalid chat ID", slog.String("id", idStr))
				continue
			}
			wg.Add(1)
			go func(chatID int64) {
				defer wg.Done()
				s.streamChat(ctx, client.API(), dispatcher, joinAs, peers, chatID, streamURL)
			}(id)
		}
		wg.Wait()
		return nil
	}); err != nil && !errors.Is(err, context.Canceled) {
		s.logger.Error("Telegram streamer exited", slog.String("error", err.Error()))
	}

	// Restart with backoff if still enabled.
	s.mutex.RLock()
	enabled := s.config.Enabled
	s.mutex.RUnlock()

	if enabled {
		delay := 5 * time.Second
		s.logger.Info("Restarting Telegram streamer", slog.Duration("delay", delay))
		time.Sleep(delay)
		if err := s.Restart(); err != nil {
			s.logger.Error("Telegram streamer restart failed", slog.String("error", err.Error()))
		}
	}
}

func (s *Service) streamChat(ctx context.Context, api *tg.Client, dispatcher tg.UpdateDispatcher, joinAs *tg.InputPeerUser, peers map[int64]tg.InputPeerClass, chatID int64, streamURL string) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		call, err := resolveGroupCall(ctx, api, peers, chatID)
		if err != nil {
			s.logger.Warn("No active group call for chat", slog.Int64("chat", chatID), slog.String("error", err.Error()))
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
				continue
			}
		}

		gc := calls.NewGroupCall(api, calls.Options{})
		gc.Register(dispatcher)
		gc.OnConnected(func() {
			s.logger.Info("Joined voice chat", slog.Int64("chat", chatID))
		})
		gc.OnDisconnected(func() {
			s.logger.Info("Left voice chat", slog.Int64("chat", chatID))
		})
		gc.OnParticipants(func(participants []tg.GroupCallParticipant) {
			for _, p := range participants {
				if user, ok := p.Peer.(*tg.PeerUser); ok && user.UserID == joinAs.UserID {
					attrs := []slog.Attr{
						slog.Int64("user", user.UserID),
						slog.Bool("muted", p.Muted),
						slog.Bool("canSelfUnmute", p.CanSelfUnmute),
						slog.Bool("mutedByYou", p.MutedByYou),
						slog.Bool("volumeByAdmin", p.VolumeByAdmin),
					}
					if volume, ok := p.GetVolume(); ok {
						attrs = append(attrs, slog.Int("volume", volume))
					}
					level := slog.LevelDebug
					msg := "self participant update"
					if p.Muted {
						level = slog.LevelWarn
						msg = "self participant is muted"
					}
					s.logger.LogAttrs(ctx, level, msg, attrs...)
					break
				}
			}
		})

		if err := gc.Join(ctx, call, joinAs); err != nil {
			s.logger.Warn("Failed to join voice chat", slog.Int64("chat", chatID), slog.String("error", err.Error()))
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}

		// Explicitly unmute ourselves; some group calls may mute new joiners by
		// default even if the join request doesn't ask for it.
		editReq := &tg.PhoneEditGroupCallParticipantRequest{
			Call:        call,
			Participant: joinAs,
		}
		editReq.SetMuted(false)
		if _, err := api.PhoneEditGroupCallParticipant(ctx, editReq); err != nil {
			s.logger.Warn("Failed to unmute in voice chat", slog.Int64("chat", chatID), slog.String("error", err.Error()))
		}

		streamErr := streamAudio(ctx, s.logger.With(slog.Int64("chat", chatID)), gc.WriteAudio, streamURL)
		if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
			s.logger.Warn("Audio stream ended", slog.Int64("chat", chatID), slog.String("error", streamErr.Error()))
		}

		if err := gc.Leave(ctx); err != nil {
			s.logger.Warn("Failed to leave voice chat", slog.Int64("chat", chatID), slog.String("error", err.Error()))
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func resolveGroupCall(ctx context.Context, api *tg.Client, peers map[int64]tg.InputPeerClass, chatID int64) (*tg.InputGroupCall, error) {
	peerID, isChannel := normalizeChatID(chatID)
	peer, ok := peers[peerID]
	if !ok {
		return nil, fmt.Errorf("chat %d (resolved peer %d, channel=%v) not found in peer list; ensure the account is a member and the chat has at least one message", chatID, peerID, isChannel)
	}

	switch p := peer.(type) {
	case *tg.InputPeerChannel:
		full, err := api.ChannelsGetFullChannel(ctx, &tg.InputChannel{
			ChannelID:  p.ChannelID,
			AccessHash: p.AccessHash,
		})
		if err != nil {
			return nil, err
		}
		cf, ok := full.FullChat.(*tg.ChannelFull)
		if !ok {
			return nil, fmt.Errorf("unexpected full chat %T", full.FullChat)
		}
		call, ok := cf.Call.(*tg.InputGroupCall)
		if !ok {
			return nil, errors.New("no active group call")
		}
		return call, nil
	case *tg.InputPeerChat:
		full, err := api.MessagesGetFullChat(ctx, p.ChatID)
		if err != nil {
			return nil, err
		}
		cf, ok := full.FullChat.(*tg.ChatFull)
		if !ok {
			return nil, fmt.Errorf("unexpected full chat %T", full.FullChat)
		}
		call, ok := cf.Call.(*tg.InputGroupCall)
		if !ok {
			return nil, errors.New("no active group call")
		}
		return call, nil
	default:
		return nil, fmt.Errorf("unsupported peer type %T", peer)
	}
}

func fetchInputPeers(ctx context.Context, api *tg.Client) (map[int64]tg.InputPeerClass, error) {
	peers := make(map[int64]tg.InputPeerClass)

	offsetDate := 0
	offsetID := 0
	offsetPeer := tg.InputPeerClass(&tg.InputPeerEmpty{})

	for {
		dialogs, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetDate: offsetDate,
			OffsetID:   offsetID,
			OffsetPeer: offsetPeer,
			Limit:      200,
		})
		if err != nil {
			return nil, err
		}

		var dslice []tg.DialogClass
		var chats []tg.ChatClass
		var messages []tg.MessageClass
		switch d := dialogs.(type) {
		case *tg.MessagesDialogs:
			dslice = d.Dialogs
			chats = d.Chats
			messages = d.Messages
		case *tg.MessagesDialogsSlice:
			dslice = d.Dialogs
			chats = d.Chats
			messages = d.Messages
		default:
			return nil, fmt.Errorf("unexpected dialogs type %T", dialogs)
		}
		if len(dslice) == 0 {
			break
		}

		chatMap := make(map[int64]tg.ChatClass)
		for _, c := range chats {
			chatMap[c.GetID()] = c
		}

		for _, dialog := range dslice {
			d, ok := dialog.(*tg.Dialog)
			if !ok {
				continue
			}
			peer, ok := d.Peer.(tg.PeerClass)
			if !ok {
				continue
			}
			switch p := peer.(type) {
			case *tg.PeerChannel:
				if ch, ok := chatMap[p.ChannelID].(*tg.Channel); ok {
					peers[p.ChannelID] = &tg.InputPeerChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}
				}
			case *tg.PeerChat:
				peers[p.ChatID] = &tg.InputPeerChat{ChatID: p.ChatID}
			}
		}

		last, ok := dslice[len(dslice)-1].(*tg.Dialog)
		if !ok {
			break
		}
		msgDate, msgID := topMessageDate(messages, last.TopMessage)
		if msgID == 0 {
			break
		}
		offsetDate = msgDate
		offsetID = msgID
		// Use the last dialog's peer as the next offset peer if we have it.
		var lastPeerID int64
		switch p := last.Peer.(type) {
		case *tg.PeerChannel:
			lastPeerID = p.ChannelID
		case *tg.PeerChat:
			lastPeerID = p.ChatID
		case *tg.PeerUser:
			lastPeerID = p.UserID
		}
		if nextPeer, ok := peers[lastPeerID]; ok {
			offsetPeer = nextPeer
		} else {
			break
		}
	}

	return peers, nil
}

// normalizeChatID converts a Telegram bot API chat ID to an MTProto peer ID.
//
// Telegram uses three ID spaces in the bot API:
//   - Users: positive user_id
//   - Legacy groups: -chat_id
//   - Channels/supergroups: -1000000000000 - channel_id
//
// gotd stores channel and chat peers by their bare MTProto ID, so configured
// -100... supergroup IDs must be converted before looking them up in the peer
// map built from MessagesGetDialogs.
func normalizeChatID(botAPIID int64) (peerID int64, isChannel bool) {
	if botAPIID >= 0 {
		return botAPIID, false
	}
	if botAPIID <= -1000000000000 {
		return -(botAPIID + 1000000000000), true
	}
	return -botAPIID, false
}

func topMessageDate(messages []tg.MessageClass, topMsgID int) (int, int) {
	if topMsgID == 0 {
		return 0, 0
	}
	for _, m := range messages {
		msg, ok := m.(*tg.Message)
		if !ok {
			continue
		}
		if msg.ID == topMsgID {
			return msg.Date, msg.ID
		}
	}
	return 0, 0
}

// streamAudio transcodes the stream URL to Opus with ffmpeg and feeds it to write
// as RTP packets, pacing in real time. It blocks until ctx is cancelled or the
// source ends.
func streamAudio(ctx context.Context, log *slog.Logger, write func(*rtp.Packet) error, streamURL string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "warning",
		"-fflags", "+nobuffer",
		"-user_agent", "Airstation/1.0 (ffmpeg)",
		"-f", "hls",
		"-live_start_index", "-3",
		"-i", streamURL,
		"-vn",
		"-ac", "2", "-ar", "48000",
		"-c:a", "libopus", "-b:a", "64k", "-application", "voip",
		"-frame_duration", "20",
		"-f", "ogg", "pipe:1",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("ffmpeg stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("ffmpeg stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}
	log.Info("ffmpeg started", slog.String("url", streamURL))

	// Forward ffmpeg stderr to our logger so warnings/errors are visible.
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			log.Warn("ffmpeg", slog.String("stderr", scanner.Text()))
		}
	}()

	defer func() {
		if err := cmd.Wait(); err != nil {
			log.Warn("ffmpeg exited", slog.String("error", err.Error()))
		}
	}()

	ogg := &oggDemuxer{r: bufio.NewReader(stdout)}
	for range 2 {
		if _, err := ogg.next(); err != nil {
			return fmt.Errorf("read opus header: %w", err)
		}
	}

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	seq := uint16(rand.Uint32()) //nolint:gosec
	ts := rand.Uint32()          //nolint:gosec
	opusSamples := uint32(48000 / 1000 * 20)
	marker := true
	for {
		frame, err := ogg.next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read opus frame: %w", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		seq++
		ts += opusSamples
		if seq%250 == 0 {
			log.Debug("sending audio frames", slog.Uint64("seq", uint64(seq)), slog.Int("payload", len(frame)))
		}
		if err := write(&rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				Marker:         marker,
				PayloadType:    111,
				SequenceNumber: seq,
				Timestamp:      ts,
			},
			Payload: frame,
		}); err != nil {
			if errors.Is(err, io.ErrClosedPipe) {
				return nil
			}
			return fmt.Errorf("write rtp: %w", err)
		}
		if marker {
			log.Info("first audio RTP frame sent")
		}
		marker = false
	}
}

type oggDemuxer struct {
	r     io.Reader
	queue [][]byte
	cur   []byte
}

func (d *oggDemuxer) next() ([]byte, error) {
	for len(d.queue) == 0 {
		if err := d.readPage(); err != nil {
			return nil, err
		}
	}
	pkt := d.queue[0]
	d.queue = d.queue[1:]
	return pkt, nil
}

func (d *oggDemuxer) readPage() error {
	var header [27]byte
	if _, err := io.ReadFull(d.r, header[:]); err != nil {
		return err
	}
	if string(header[0:4]) != "OggS" {
		return errors.New("invalid ogg capture pattern")
	}

	segments := int(header[26])
	table := make([]byte, segments)
	if _, err := io.ReadFull(d.r, table); err != nil {
		return err
	}
	total := 0
	for _, n := range table {
		total += int(n)
	}
	data := make([]byte, total)
	if _, err := io.ReadFull(d.r, data); err != nil {
		return err
	}

	off := 0
	for _, n := range table {
		d.cur = append(d.cur, data[off:off+int(n)]...)
		off += int(n)
		if n < 255 {
			d.queue = append(d.queue, d.cur)
			d.cur = nil
		}
	}
	return nil
}
