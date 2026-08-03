package telegram

import (
	"errors"
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

func (f *fakeStore) DeleteStationProperty(key string) error {
	if f.props == nil {
		return errors.New("property not found")
	}
	if _, ok := f.props[key]; !ok {
		return errors.New("property not found")
	}
	delete(f.props, key)
	return nil
}

func TestServiceLoad(t *testing.T) {
	store := &fakeStore{
		props: map[string]string{
			propEnabled:       "true",
			propAPIID:         "12345",
			propAPIHash:       "abc",
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
	if cfg.SessionString != "session" {
		t.Errorf("expected SessionString=session, got %q", cfg.SessionString)
	}
	if len(cfg.ChatIDs) != 2 || cfg.ChatIDs[0] != "-1001" || cfg.ChatIDs[1] != "-1002" {
		t.Errorf("unexpected chat IDs: %v", cfg.ChatIDs)
	}

	pub := svc.PublicConfig()
	if !pub.HasAPIID || !pub.HasAPIHash || !pub.HasSession {
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

	_, err = svc.EditConfig(Config{Enabled: true, APIID: 1, APIHash: "hash", SessionString: ""})
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
		Enabled:       true,
		APIID:         123,
		APIHash:       "hash",
		SessionString: "sess",
		StreamURL:     "http://localhost/stream",
		ChatIDs:       []string{"-1001", "-1002"},
	})
	if err != nil {
		t.Fatalf("EditConfig failed: %v", err)
	}

	if !cfg.Enabled || !cfg.HasAPIID || !cfg.HasAPIHash || !cfg.HasSession || len(cfg.ChatIDs) != 2 {
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

func TestServiceStopNoProcess(t *testing.T) {
	store := &fakeStore{props: make(map[string]string)}
	svc := NewService(store, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := svc.Stop(); err != nil {
		t.Fatalf("Stop with no process should not error: %v", err)
	}
}

func TestNormalizeChatID(t *testing.T) {
	tests := []struct {
		input    int64
		wantID   int64
		wantChan bool
	}{
		{12345, 12345, false},
		{-12345, 12345, false},
		{-1003548656968, 3548656968, true},
		{-1001, 1001, false},
	}
	for _, tt := range tests {
		gotID, gotChan := normalizeChatID(tt.input)
		if gotID != tt.wantID || gotChan != tt.wantChan {
			t.Errorf("normalizeChatID(%d) = (%d, %v), want (%d, %v)", tt.input, gotID, gotChan, tt.wantID, tt.wantChan)
		}
	}
}
