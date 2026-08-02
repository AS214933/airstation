package telegram

import (
	"context"
	"encoding/base64"
	"sync"
)

// memorySessionStorage implements telegram.SessionStorage in memory.
type memorySessionStorage struct {
	mu   sync.Mutex
	data []byte
}

func (m *memorySessionStorage) LoadSession(context.Context) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]byte, len(m.data))
	copy(out, m.data)
	return out, nil
}

func (m *memorySessionStorage) StoreSession(_ context.Context, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data = make([]byte, len(data))
	copy(m.data, data)
	return nil
}

func (m *memorySessionStorage) bytes() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]byte, len(m.data))
	copy(out, m.data)
	return out
}

// persistedSessionStorage implements telegram.SessionStorage backed by the
// station property store. The session data is stored base64-encoded under
// propSessionString.
type persistedSessionStorage struct {
	s *Service
}

func (p *persistedSessionStorage) LoadSession(context.Context) ([]byte, error) {
	p.s.mutex.RLock()
	encoded := p.s.config.SessionString
	p.s.mutex.RUnlock()
	if encoded == "" {
		return nil, nil
	}
	return base64.StdEncoding.DecodeString(encoded)
}

func (p *persistedSessionStorage) StoreSession(_ context.Context, data []byte) error {
	encoded := base64.StdEncoding.EncodeToString(data)
	if _, err := p.s.store.UpsertStationProperty(propSessionString, encoded); err != nil {
		return err
	}
	p.s.mutex.Lock()
	p.s.config.SessionString = encoded
	p.s.mutex.Unlock()
	return nil
}
