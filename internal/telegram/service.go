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

// ErrPasswordNeeded is returned by SignInUserbot when the account requires
// a 2FA password to complete sign-in.
var ErrPasswordNeeded = errors.New("2FA password required")

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
	store        Store
	logger       *slog.Logger
	workDir      string
	loginWorkDir string
	pythonBin    string

	mutex         sync.RWMutex
	loginMutex    sync.Mutex
	config        Config
	proc          StreamRunner
	procCtx       context.Context
	procCancel    context.CancelFunc
	procErr       error
	runnerFactory StreamRunnerFactory
}

// NewService creates a new Telegram voice stream service.
func NewService(store Store, log *slog.Logger) *Service {
	workDir := filepath.Join("storage", "telegram")
	return &Service{
		store:         store,
		logger:        log.WithGroup("telegram"),
		workDir:       workDir,
		loginWorkDir:  filepath.Join(workDir, "login"),
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
			return PublicConfig{}, errors.New("Telegram session string is required; log in via the userbot login flow first")
		}
		if len(newConfig.ChatIDs) == 0 {
			return PublicConfig{}, errors.New("at least one Telegram chat ID is required")
		}
		if strings.TrimSpace(newConfig.StreamURL) == "" {
			return PublicConfig{}, errors.New("stream URL is required")
		}
	}

	if _, err := s.store.UpsertStationProperty(propEnabled, strconv.FormatBool(newConfig.Enabled)); err != nil {
		return PublicConfig{}, err
	}
	if _, err := s.store.UpsertStationProperty(propAPIID, strconv.Itoa(newConfig.APIID.Int())); err != nil {
		return PublicConfig{}, err
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
	newConfig.StreamURL = strings.TrimSpace(newConfig.StreamURL)

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

// Test validates Telegram credentials by attempting a quick Pyrogram connection.
// It spawns a short-lived Python one-liner using the configured credentials.
// Empty fields are merged with the stored configuration so the UI can test
// credentials without sending the secret values back from the client.
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
		return errors.New("Telegram session string is required")
	}

	script := `
import asyncio, json, sys
from pyrogram import Client

async def main():
    client = Client(
        name="airstation_test",
        api_id=int(sys.argv[1]),
        api_hash=sys.argv[2],
        session_string=sys.argv[3],
        in_memory=True,
        no_updates=True,
    )
    await client.start()
    me = await client.get_me()
    print(json.dumps({"id": me.id, "username": me.username or "", "first_name": me.first_name or "", "is_bot": me.is_bot}))
    await client.stop()

asyncio.run(main())
`
	cmd := exec.Command(s.pythonBin, "-c", script, strconv.Itoa(cfg.APIID.Int()), cfg.APIHash, cfg.SessionString)
	var out bytes.Buffer
	var errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("telegram credential test failed: %v: %s", err, errBuf.String())
	}

	var me struct {
		IsBot bool `json:"is_bot"`
	}
	if err := json.Unmarshal(out.Bytes(), &me); err != nil {
		return fmt.Errorf("parsing telegram account info: %w", err)
	}
	if me.IsBot {
		return errors.New("bot accounts cannot be used for Telegram voice streaming; use a user account")
	}
	return nil
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

func (s *Service) loginScriptCmd(ctx context.Context, args ...string) (*exec.Cmd, error) {
	if err := os.MkdirAll(s.loginWorkDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating telegram login workdir failed: %w", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("getting working directory failed: %w", err)
	}

	script := filepath.Join(cwd, "tools", "telegram_login.py")
	allArgs := append([]string{script}, args...)
	cmd := exec.CommandContext(ctx, s.pythonBin, allArgs...)
	cmd.Dir = s.loginWorkDir
	return cmd, nil
}

// loadLoginState reads the persisted userbot login state.
func (s *Service) loadLoginState() (phone, phoneCodeHash, step string, err error) {
	props, err := s.store.StationProperties()
	if err != nil {
		return "", "", "", err
	}
	for _, p := range props {
		switch p.Key {
		case propLoginPhone:
			phone = p.Value
		case propLoginPhoneCodeHash:
			phoneCodeHash = p.Value
		case propLoginStep:
			step = p.Value
		}
	}
	return phone, phoneCodeHash, step, nil
}

// saveLoginState persists the userbot login state.
func (s *Service) saveLoginState(phone, phoneCodeHash, step string) error {
	if _, err := s.store.UpsertStationProperty(propLoginPhone, phone); err != nil {
		return err
	}
	if _, err := s.store.UpsertStationProperty(propLoginPhoneCodeHash, phoneCodeHash); err != nil {
		return err
	}
	if _, err := s.store.UpsertStationProperty(propLoginStep, step); err != nil {
		return err
	}
	return nil
}

// clearLoginState removes the persisted userbot login state.
func (s *Service) clearLoginState() error {
	_ = s.store.DeleteStationProperty(propLoginPhone)
	_ = s.store.DeleteStationProperty(propLoginPhoneCodeHash)
	_ = s.store.DeleteStationProperty(propLoginStep)
	return nil
}

// SendLoginCode requests a Telegram login code for the given phone number.
// It returns the phone_code_hash needed for SignInUserbot and persists the
// login state so the 2FA step can continue later.
func (s *Service) SendLoginCode(phone string, apiID int, apiHash string) (string, error) {
	s.loginMutex.Lock()
	defer s.loginMutex.Unlock()

	phone = strings.TrimSpace(phone)
	if phone == "" {
		return "", errors.New("phone number is required")
	}

	apiID, apiHash, err := s.resolveAPICredentials(apiID, apiHash)
	if err != nil {
		return "", err
	}

	if err := s.clearLoginState(); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd, err := s.loginScriptCmd(ctx, "send-code", strconv.Itoa(apiID), apiHash, phone)
	if err != nil {
		return "", err
	}

	var out bytes.Buffer
	var errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("telegram send-login-code failed: %w: %s", err, errBuf.String())
	}

	var resp LoginCodeResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		return "", fmt.Errorf("parsing login code response: %w", err)
	}
	if resp.PhoneCodeHash == "" {
		return "", errors.New("login code response did not include phone_code_hash")
	}

	if err := s.saveLoginState(phone, resp.PhoneCodeHash, "code_sent"); err != nil {
		return "", err
	}

	return resp.PhoneCodeHash, nil
}

// SignInUserbot completes a userbot login with the verification code and optional
// 2FA password. If the account requires a password, it returns ErrPasswordNeeded
// and the caller must retry with the password. On success the resulting session
// string is persisted and the streamer is restarted so the new session takes
// effect immediately.
func (s *Service) SignInUserbot(phone, phoneCodeHash, code, password string, apiID int, apiHash string) error {
	s.loginMutex.Lock()
	defer s.loginMutex.Unlock()

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

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// If we already know a password is required, finish the login with the
	// stored session instead of trying to consume the code again.
	_, storedHash, step, err := s.loadLoginState()
	if err != nil {
		return err
	}
	awaitingPassword := step == "awaiting_password" && storedHash == phoneCodeHash

	var cmd *exec.Cmd
	if awaitingPassword && password != "" {
		cmd, err = s.loginScriptCmd(ctx, "check-password", strconv.Itoa(apiID), apiHash, phone, "--password", password)
	} else {
		args := []string{
			"sign-in",
			strconv.Itoa(apiID),
			apiHash,
			phone,
			phoneCodeHash,
			code,
		}
		if password != "" {
			args = append(args, "--password", password)
		}
		cmd, err = s.loginScriptCmd(ctx, args...)
	}
	if err != nil {
		return err
	}

	var out bytes.Buffer
	var errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("telegram sign-in failed: %w: %s", err, errBuf.String())
	}

	var result struct {
		NeedsPassword bool   `json:"needsPassword"`
		SessionString string `json:"sessionString"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		return fmt.Errorf("parsing sign-in response: %w", err)
	}

	if result.NeedsPassword {
		if err := s.saveLoginState(phone, phoneCodeHash, "awaiting_password"); err != nil {
			return err
		}
		return ErrPasswordNeeded
	}
	if result.SessionString == "" {
		return errors.New("sign-in did not return a session string")
	}

	if _, err := s.store.UpsertStationProperty(propSessionString, result.SessionString); err != nil {
		return err
	}

	s.mutex.Lock()
	s.config.SessionString = result.SessionString
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

// Start spawns the Python streamer subprocess if enabled and not already running.
func (s *Service) Start() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if !s.config.Enabled {
		return nil
	}
	if s.config.APIID == 0 || s.config.APIHash == "" || s.config.SessionString == "" || len(s.config.ChatIDs) == 0 {
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
