package telegram

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"testing"

	"github.com/annihilatorrrr/gotgcall"
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
	if cfg.StreamURL != "" {
		t.Errorf("expected legacy StreamURL to be ignored, got %q", cfg.StreamURL)
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

	// Empty stream URL is allowed; it falls back to the local playlist.
	_, err = svc.EditConfig(Config{Enabled: true, APIID: 1, APIHash: "hash", SessionString: "sess", ChatIDs: []string{"-1001"}, StreamURL: ""})
	if err != nil {
		t.Errorf("unexpected error for empty stream URL: %v", err)
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

func TestGroupCallJoinPayloadHasRequiredMediaMetadata(t *testing.T) {
	media, err := gotgcall.New(gotgcall.WithFFmpegPath("true"))
	if err != nil {
		t.Fatalf("create media transport: %v", err)
	}
	defer func() {
		if err := media.Close(); err != nil {
			t.Errorf("close media transport: %v", err)
		}
	}()

	raw, err := media.CreateCall(-1003548656968)
	if err != nil {
		t.Fatalf("create call: %v", err)
	}
	raw, err = normalizeGroupCallJoinParams(raw)
	if err != nil {
		t.Fatalf("normalize join payload: %v", err)
	}

	var payload struct {
		SSRC         int32 `json:"ssrc"`
		SSRCGroups   []any `json:"ssrc-groups"`
		PayloadTypes []struct {
			ID        int    `json:"id"`
			Name      string `json:"name"`
			Clockrate int    `json:"clockrate"`
			Channels  int    `json:"channels"`
		} `json:"payload-types"`
		RTPHdrExts []struct {
			ID  int    `json:"id"`
			URI string `json:"uri"`
		} `json:"rtp-hdrexts"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("decode join payload: %v", err)
	}
	if payload.SSRC == 0 {
		t.Error("join payload has no audio SSRC")
	}
	if payload.SSRCGroups == nil {
		t.Error("join payload must include an empty ssrc-groups array for audio-only calls")
	}

	foundOpus := false
	for _, codec := range payload.PayloadTypes {
		if codec.ID == 111 && codec.Name == "opus" && codec.Clockrate == 48000 && codec.Channels == 2 {
			foundOpus = true
			break
		}
	}
	if !foundOpus {
		t.Errorf("join payload has no valid Opus codec: %+v", payload.PayloadTypes)
	}

	requiredExtensions := map[string]bool{
		"urn:ietf:params:rtp-hdrext:ssrc-audio-level":                               false,
		"http://www.webrtc.org/experiments/rtp-hdrext/abs-send-time":                false,
		"http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01": false,
		"urn:ietf:params:rtp-hdrext:sdes:mid":                                       false,
	}
	for _, extension := range payload.RTPHdrExts {
		if extension.ID > 0 {
			if _, ok := requiredExtensions[extension.URI]; ok {
				requiredExtensions[extension.URI] = true
			}
		}
	}
	for uri, found := range requiredExtensions {
		if !found {
			t.Errorf("join payload is missing required RTP extension %q", uri)
		}
	}
}

func TestNormalizeGroupCallJoinParamsSignsSSRC(t *testing.T) {
	normalized, err := normalizeGroupCallJoinParams(`{"ssrc":4294967295,"ufrag":"test","payload-types":[]}`)
	if err != nil {
		t.Fatalf("normalize join payload: %v", err)
	}

	var payload struct {
		SSRC int32 `json:"ssrc"`
	}
	if err := json.Unmarshal([]byte(normalized), &payload); err != nil {
		t.Fatalf("decode normalized payload: %v", err)
	}
	if payload.SSRC != -1 {
		t.Fatalf("SSRC = %d, want -1", payload.SSRC)
	}
}
