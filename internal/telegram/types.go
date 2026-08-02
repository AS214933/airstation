package telegram

import "github.com/cheatsnake/airstation/internal/station"

// Store provides access to persisted station properties.
type Store interface {
	StationProperties() ([]*station.Property, error)
	UpsertStationProperty(key, value string) (*station.Property, error)
}

// Config holds the full Telegram voice stream configuration, including secrets.
// Authentication can be done either with a Pyrogram session string (user or bot
// account) or with a Bot API token, which is used to create an MTProto bot session.
type Config struct {
	Enabled       bool     `json:"enabled"`
	APIID         int      `json:"apiID"`
	APIHash       string   `json:"apiHash"`
	BotToken      string   `json:"botToken"`
	SessionString string   `json:"sessionString"`
	StreamURL     string   `json:"streamURL"`
	ChatIDs       []string `json:"chatIDs"`
}

// PublicConfig is the configuration exposed through public API endpoints.
// Secrets are never returned.
type PublicConfig struct {
	Enabled     bool     `json:"enabled"`
	StreamURL   string   `json:"streamURL"`
	ChatIDs     []string `json:"chatIDs"`
	HasAPIID    bool     `json:"hasAPIID"`
	HasAPIHash  bool     `json:"hasAPIHash"`
	HasBotToken bool     `json:"hasBotToken"`
	HasSession  bool     `json:"hasSession"`
}
