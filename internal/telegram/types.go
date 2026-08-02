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
	DeleteStationProperty(key string) error
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
// Voice streaming requires a Pyrogram user session string; bot tokens are not
// supported because Telegram does not allow bots to join voice chats.
type Config struct {
	Enabled       bool      `json:"enabled"`
	APIID         IntString `json:"apiID"`
	APIHash       string    `json:"apiHash"`
	SessionString string    `json:"sessionString"`
	StreamURL     string    `json:"streamURL"`
	ChatIDs       []string  `json:"chatIDs"`
}

// PublicConfig is the configuration exposed through public API endpoints.
// Secrets are never returned.
type PublicConfig struct {
	Enabled    bool     `json:"enabled"`
	StreamURL  string   `json:"streamURL"`
	ChatIDs    []string `json:"chatIDs"`
	HasAPIID   bool     `json:"hasAPIID"`
	HasAPIHash bool     `json:"hasAPIHash"`
	HasSession bool     `json:"hasSession"`
}

// LoginCodeResponse is returned after a verification code has been requested.
type LoginCodeResponse struct {
	PhoneCodeHash string `json:"phoneCodeHash"`
}
