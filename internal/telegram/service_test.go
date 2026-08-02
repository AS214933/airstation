package telegram

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/cheatsnake/airstation/internal/station"
)

type fakeStore struct {
	props map[string]string
}

func (f *fakeStore) StationProperties() ([]*station.Property, error) {
	result := make([]*station.Property, 0, len(f.props))
	for k, v := range f.props {
		result = append(result, &station.Property{Key: k, Value: v})
	}
	return result, nil
}

func (f *fakeStore) UpsertStationProperty(key, value string) (*station.Property, error) {
	if f.props == nil {
		f.props = make(map[string]string)
	}
	f.props[key] = value
	return &station.Property{Key: key, Value: value}, nil
}

type fakeRunner struct {
	started   bool
	waitErr   error
	process   *os.Process
	waitCalls int
}

func (f *fakeRunner) Start() error {
	f.started = true
	return nil
}

func (f *fakeRunner) Wait() error {
	f.waitCalls++
	return f.waitErr
}

func (f *fakeRunner) Process() *os.Process {
	return f.process
}

func TestServiceLoad(t *testing.T) {
	store := &fakeStore{
		props: map[string]string{
			propEnabled:       "true",
			propAPIID:         "12345",
			propAPIHash:       "abc",
			propBotToken:      "bot:token",
			propSessionString: "session",
			propStreamURL:     "http://localhost/stream",
			propChatIDs:       " -1001 , -1002 ",
		},
	}

	svc := NewService(store, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := svc.Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	cfg := svc.Config()
	if !cfg.Enabled {
		t.Error("expected enabled")
	}
	if cfg.APIID != 12345 {
		t.Errorf("expected APIID=12345, got %d", cfg.APIID)
	}
	if cfg.APIHash != "abc" {
		t.Errorf("expected APIHash=abc, got %q", cfg.APIHash)
	}
	if cfg.BotToken != "bot:token" {
		t.Errorf("expected BotToken=bot:token, got %q", cfg.BotToken)
	}
	if len(cfg.ChatIDs) != 2 || cfg.ChatIDs[0] != "-1001" || cfg.ChatIDs[1] != "-1002" {
		t.Errorf("unexpected chat IDs: %v", cfg.ChatIDs)
	}

	pub := svc.PublicConfig()
	if !pub.HasAPIID || !pub.HasAPIHash || !pub.HasBotToken || !pub.HasSession {
		t.Errorf("expected all secrets present: %+v", pub)
	}
}

func TestServiceEditConfigValidation(t *testing.T) {
	store := &fakeStore{props: make(map[string]string)}
	svc := NewService(store, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	_, err := svc.EditConfig(Config{Enabled: true, APIID: 0})
	if err == nil {
		t.Error("expected error for missing API ID")
	}

	_, err = svc.EditConfig(Config{Enabled: true, APIID: 1, APIHash: "hash"})
	if err == nil {
		t.Error("expected error for missing auth")
	}

	_, err = svc.EditConfig(Config{Enabled: true, APIID: 1, APIHash: "hash", ChatIDs: []string{"abc"}, SessionString: "sess", StreamURL: "http://x"})
	if err == nil {
		t.Error("expected error for invalid chat ID")
	}

	_, err = svc.EditConfig(Config{Enabled: true, APIID: 1, APIHash: "hash", SessionString: "sess", ChatIDs: []string{"abc"}, StreamURL: ""})
	if err == nil {
		t.Error("expected error for empty stream URL")
	}
}

func TestServiceEditConfigPersists(t *testing.T) {
	store := &fakeStore{props: make(map[string]string)}
	svc := NewService(store, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	cfg, err := svc.EditConfig(Config{
		Enabled:   true,
		APIID:     123,
		APIHash:   "hash",
		BotToken:  "bot:token",
		StreamURL: "http://localhost/stream",
		ChatIDs:   []string{"-1001", "-1002"},
	})
	if err != nil {
		t.Fatalf("EditConfig failed: %v", err)
	}

	if !cfg.Enabled || !cfg.HasAPIID || !cfg.HasAPIHash || !cfg.HasBotToken || len(cfg.ChatIDs) != 2 {
		t.Errorf("unexpected public config: %+v", cfg)
	}

	if store.props[propEnabled] != "true" {
		t.Errorf("expected enabled=true, got %q", store.props[propEnabled])
	}
	if store.props[propAPIID] != "123" {
		t.Errorf("expected APIID=123, got %q", store.props[propAPIID])
	}
	if store.props[propChatIDs] != "-1001,-1002" {
		t.Errorf("unexpected chat IDs stored: %q", store.props[propChatIDs])
	}
}

func TestServiceStartStop(t *testing.T) {
	store := &fakeStore{props: map[string]string{
		propEnabled:   "true",
		propAPIID:     "123",
		propAPIHash:   "hash",
		propBotToken:  "bot:token",
		propStreamURL: "http://localhost/stream",
		propChatIDs:   "-1001",
	}}
	svc := NewService(store, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := svc.Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	runner := &fakeRunner{}
	svc.runnerFactory = func(ctx context.Context, cfg Config, workDir, pythonBin string) (StreamRunner, error) {
		return runner, nil
	}

	if err := svc.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if !runner.started {
		t.Error("runner should have been started")
	}

	if err := svc.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
}

func TestServiceStopNoProcess(t *testing.T) {
	store := &fakeStore{props: make(map[string]string)}
	svc := NewService(store, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := svc.Stop(); err != nil {
		t.Fatalf("Stop with no process should not error: %v", err)
	}
}
