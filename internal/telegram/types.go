package telegram

import (
	"encoding/json"
	"strconv"

	"github.com/cheatsnake/airstation/internal/station"
)

// Store provides access to persisted station properties.
type Store interface {
	StationProperties() ([]*station.Property, error)
	UpsertStationProperty(key, value string) (*station.Property, error)
}

// IntString is an integer that can be decoded from either a JSON number or a
// JSON string, so that HTML number inputs do not have to be normalized before
// sending.
type IntString int

// UnmarshalJSON implements json.Unmarshaler.
func (i *IntString) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		v, err := strconv.Atoi(s)
		if err != nil {
			return err
		}
		*i = IntString(v)
		return nil
	}

	var n int
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*i = IntString(n)
	return nil
}

// Int returns the integer value.
func (i IntString) Int() int {
	return int(i)
}

// Config holds the full Telegram voice stream configuration, including secrets.
// Authentication can be done either with a Pyrogram session string (user or bot
// account) or with a Bot API token, which is used to create an MTProto bot session.
type Config struct {
	Enabled       bool      `json:"enabled"`
	APIID         IntString `json:"apiID"`
	APIHash       string    `json:"apiHash"`
	BotToken      string    `json:"botToken"`
	SessionString string    `json:"sessionString"`
	StreamURL     string    `json:"streamURL"`
	ChatIDs       []string  `json:"chatIDs"`
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
