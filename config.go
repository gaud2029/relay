package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"maunium.net/go/mautrix/id"
)

// Session is what `relay login` leaves behind for the daemon: the account's
// credentials and the key that encrypts the crypto store at rest.
type Session struct {
	Homeserver  string      `json:"homeserver"`
	UserID      id.UserID   `json:"user_id"`
	DeviceID    id.DeviceID `json:"device_id"`
	AccessToken string      `json:"access_token"`
	PickleKey   string      `json:"pickle_key"`
}

var errNoSession = errors.New("pas de session : lance `relay login` d'abord")

func dataDir() string {
	if dir := os.Getenv("RELAY_HOME"); dir != "" {
		return dir
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "relay")
}

func sessionPath() string { return filepath.Join(dataDir(), "session.json") }
func dbPath() string      { return filepath.Join(dataDir(), "relay.db") }
func roomsPath() string   { return filepath.Join(dataDir(), "rooms.json") }

func socketPath() string {
	if p := os.Getenv("RELAY_SOCKET"); p != "" {
		return p
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "relay.sock")
}

func loadSession() (*Session, error) {
	data, err := os.ReadFile(sessionPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNoSession
	} else if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s illisible : %w", sessionPath(), err)
	}
	return &s, nil
}

func (s *Session) save() error {
	if err := os.MkdirAll(dataDir(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(sessionPath(), data, 0o600)
}

func newPickleKey() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}
