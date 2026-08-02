package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// StreamRunnerFactory creates a StreamRunner for the given configuration.
type StreamRunnerFactory func(ctx context.Context, cfg Config, workDir, pythonBin string) (StreamRunner, error)

// StreamRunner abstracts spawning the external Python streamer process.
// It is satisfied by *exec.Cmd in production and can be stubbed in tests.
type StreamRunner interface {
	Start() error
	Wait() error
	Process() *os.Process
}

type execCmdRunner struct {
	*exec.Cmd
}

func (r *execCmdRunner) Start() error { return r.Cmd.Start() }
func (r *execCmdRunner) Wait() error  { return r.Cmd.Wait() }
func (r *execCmdRunner) Process() *os.Process {
	return r.Cmd.Process
}

func defaultRunnerFactory(ctx context.Context, cfg Config, workDir, pythonBin string) (StreamRunner, error) {
	payload, err := json.Marshal(map[string]any{
		"api_id":         cfg.APIID,
		"api_hash":       cfg.APIHash,
		"bot_token":      cfg.BotToken,
		"session_string": cfg.SessionString,
		"stream_url":     cfg.StreamURL,
		"chat_ids":       cfg.ChatIDs,
		"workdir":        workDir,
		"log_level":      "INFO",
	})
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, pythonBin, filepath.Join("tools", "telegram_streamer.py"), "-")
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir, _ = os.Getwd()

	return &execCmdRunner{cmd}, nil
}

// Service manages Telegram voice stream configuration and the Python streamer subprocess.
type Service struct {
	store     Store
	logger    *slog.Logger
	workDir   string
	pythonBin string

	mutex         sync.RWMutex
	config        Config
	proc          StreamRunner
	procCtx       context.Context
	procCancel    context.CancelFunc
	procErr       error
	runnerFactory StreamRunnerFactory
}

// NewService creates a new Telegram voice stream service.
func NewService(store Store, log *slog.Logger) *Service {
	return &Service{
		store:         store,
		logger:        log.WithGroup("telegram"),
		workDir:       filepath.Join("storage", "telegram"),
		pythonBin:     "python3",
		runnerFactory: defaultRunnerFactory,
	}
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
		case propBotToken:
			cfg.BotToken = prop.Value
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
		Enabled:     s.config.Enabled,
		StreamURL:   s.config.StreamURL,
		ChatIDs:     append([]string(nil), s.config.ChatIDs...),
		HasAPIID:    s.config.APIID != 0,
		HasAPIHash:  s.config.APIHash != "",
		HasBotToken: s.config.BotToken != "",
		HasSession:  s.config.SessionString != "",
	}
}

// EditConfig validates and saves a new configuration, then restarts the streamer
// so changes take effect immediately.
func (s *Service) EditConfig(newConfig Config) (PublicConfig, error) {
	if newConfig.Enabled {
		if newConfig.APIID.Int() == 0 {
			return PublicConfig{}, errors.New("Telegram API ID is required")
		}
		if strings.TrimSpace(newConfig.APIHash) == "" {
			return PublicConfig{}, errors.New("Telegram API hash is required")
		}
		if strings.TrimSpace(newConfig.SessionString) == "" && strings.TrimSpace(newConfig.BotToken) == "" {
			return PublicConfig{}, errors.New("either a Telegram session string or a Bot API token is required")
		}
		if len(newConfig.ChatIDs) == 0 {
			return PublicConfig{}, errors.New("at least one Telegram chat ID is required")
		}
		if strings.TrimSpace(newConfig.StreamURL) == "" {
			return PublicConfig{}, errors.New("stream URL is required")
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

	if newConfig.StreamURL != "" {
		newConfig.StreamURL = strings.TrimSpace(newConfig.StreamURL)
	}

	enabled := "false"
	if newConfig.Enabled {
		enabled = "true"
	}

	if _, err := s.store.UpsertStationProperty(propEnabled, enabled); err != nil {
		return PublicConfig{}, err
	}
	if _, err := s.store.UpsertStationProperty(propAPIID, strconv.Itoa(newConfig.APIID.Int())); err != nil {
		return PublicConfig{}, err
	}
	if newConfig.APIHash != "" {
		if _, err := s.store.UpsertStationProperty(propAPIHash, newConfig.APIHash); err != nil {
			return PublicConfig{}, err
		}
	}
	if newConfig.BotToken != "" {
		if _, err := s.store.UpsertStationProperty(propBotToken, newConfig.BotToken); err != nil {
			return PublicConfig{}, err
		}
	}
	if newConfig.SessionString != "" {
		if _, err := s.store.UpsertStationProperty(propSessionString, newConfig.SessionString); err != nil {
			return PublicConfig{}, err
		}
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

// Test validates Telegram credentials by attempting a quick Pyrogram connection.
// It spawns a short-lived Python one-liner using the configured credentials.
func (s *Service) Test(cfg Config) error {
	if cfg.APIID.Int() == 0 {
		return errors.New("Telegram API ID is required")
	}
	if strings.TrimSpace(cfg.APIHash) == "" {
		return errors.New("Telegram API hash is required")
	}
	if strings.TrimSpace(cfg.SessionString) == "" && strings.TrimSpace(cfg.BotToken) == "" {
		return errors.New("either a Telegram session string or a Bot API token is required")
	}

	authType := "session_string"
	authValue := cfg.SessionString
	if strings.TrimSpace(cfg.SessionString) == "" {
		authType = "bot_token"
		authValue = cfg.BotToken
	}

	script := `
import asyncio, json, sys
from pyrogram import Client

async def main():
    kwargs = {
        "name": "airstation_test",
        "api_id": int(sys.argv[1]),
        "api_hash": sys.argv[2],
        "in_memory": True,
        "no_updates": True,
    }
    if sys.argv[3] == "bot_token":
        kwargs["bot_token"] = sys.argv[4]
    else:
        kwargs["session_string"] = sys.argv[4]

    client = Client(**kwargs)
    await client.start()
    me = await client.get_me()
    print(json.dumps({"id": me.id, "username": me.username or "", "first_name": me.first_name or "", "is_bot": me.is_bot}))
    await client.stop()

asyncio.run(main())
`
	cmd := exec.Command(s.pythonBin, "-c", script, strconv.Itoa(cfg.APIID.Int()), cfg.APIHash, authType, authValue)
	var out bytes.Buffer
	var errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("telegram credential test failed: %v: %s", err, errBuf.String())
	}
	return nil
}

// Start spawns the Python streamer subprocess if enabled and not already running.
func (s *Service) Start() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if !s.config.Enabled {
		return nil
	}
	if s.config.APIID == 0 || s.config.APIHash == "" || (s.config.SessionString == "" && s.config.BotToken == "") || len(s.config.ChatIDs) == 0 {
		return errors.New("Telegram voice stream is enabled but not fully configured")
	}
	if s.proc != nil && s.proc.Process() != nil {
		return nil
	}

	if err := os.MkdirAll(s.workDir, 0o755); err != nil {
		return fmt.Errorf("creating telegram workdir failed: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runner, err := s.runnerFactory(ctx, s.config, s.workDir, s.pythonBin)
	if err != nil {
		cancel()
		return fmt.Errorf("creating telegram streamer runner failed: %w", err)
	}

	if err := runner.Start(); err != nil {
		cancel()
		return fmt.Errorf("starting telegram streamer failed: %w", err)
	}

	s.proc = runner
	s.procCtx = ctx
	s.procCancel = cancel
	s.procErr = nil

	s.logger.Info("Telegram voice streamer started")

	go s.watchProcess(ctx, cancel)

	return nil
}

// Stop terminates the running streamer subprocess.
func (s *Service) Stop() error {
	s.mutex.Lock()
	proc := s.proc
	cancel := s.procCancel
	s.proc = nil
	s.procCancel = nil
	s.mutex.Unlock()

	if cancel != nil {
		cancel()
	}
	if proc != nil && proc.Process() != nil {
		_ = proc.Process().Kill()
		_ = proc.Wait()
	}
	s.logger.Info("Telegram voice streamer stopped")
	return nil
}

// Restart stops and then starts the streamer subprocess.
func (s *Service) Restart() error {
	if err := s.Stop(); err != nil {
		s.logger.Warn("Error stopping streamer during restart", slog.String("error", err.Error()))
	}
	return s.Start()
}

func (s *Service) watchProcess(ctx context.Context, cancel context.CancelFunc) {
	s.mutex.RLock()
	proc := s.proc
	s.mutex.RUnlock()

	if proc == nil {
		return
	}

	err := proc.Wait()

	s.mutex.Lock()
	if s.proc == proc {
		s.procErr = err
		s.proc = nil
		s.procCancel = nil
	}
	s.mutex.Unlock()

	if ctx.Err() == context.Canceled {
		return
	}

	if err != nil {
		s.logger.Error("Telegram streamer exited", slog.String("error", err.Error()))
	}

	// Restart with exponential backoff if still enabled.
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
