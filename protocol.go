package main

import (
	"encoding/json"

	"maunium.net/go/mautrix/id"
)

// The daemon speaks newline-delimited JSON on a Unix socket.
//
// A client sends requests:   {"id": 1, "cmd": "send", "room": "...", "body": "..."}
// and gets one reply each:   {"id": 1, "ok": true, "data": ...}
//                         or {"id": 1, "ok": false, "error": "..."}
// After "subscribe", the daemon also pushes events, which carry no id:
//                            {"event": "message", "data": {...}}
//
// Commands:
//   status                      account, device, verification and sync state
//   rooms                       joined rooms, most recent first ([]RoomInfo)
//   resolve  room               which room a name points at ({room, name})
//   history  room [limit]       last messages of a room, oldest first ([]Message)
//   send     room body          send a text message (encrypted if the room is)
//   read     room               mark the room as read up to its last message
//   verify   recovery_key       verify this device and restore the key backup
//   focus    [room]             this client shows that room now (none: no room);
//                               no desktop notification for it until it changes
//                               or the client disconnects
//   subscribe                   start receiving events
//
// "room" is a room ID, an alias, or (part of) a room's name.
//
// Events:
//   message   Message          a new message (Undecryptable when keys never came)
//   rooms     []RoomInfo       the room list changed (names, unread counts...)
//   status    Status           verification or sync state changed

type Request struct {
	ID          int64  `json:"id"`
	Cmd         string `json:"cmd"`
	Room        string `json:"room,omitempty"`
	Body        string `json:"body,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	RecoveryKey string `json:"recovery_key,omitempty"`
}

type Reply struct {
	ID    int64  `json:"id"`
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

type Push struct {
	Event string `json:"event"`
	Data  any    `json:"data"`
}

// Incoming is any line from the daemon: a Reply or a Push.
type Incoming struct {
	ID    int64           `json:"id"`
	OK    bool            `json:"ok"`
	Error string          `json:"error"`
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

type Message struct {
	ID            id.EventID `json:"id"`
	Room          id.RoomID  `json:"room"`
	RoomName      string     `json:"room_name"`
	Sender        id.UserID  `json:"sender"`
	SenderName    string     `json:"sender_name"`
	TS            int64      `json:"ts"`
	MsgType       string     `json:"msgtype,omitempty"`
	Body          string     `json:"body"`
	Edit          bool       `json:"edit,omitempty"`
	Own           bool       `json:"own,omitempty"`
	Encrypted     bool       `json:"encrypted,omitempty"`
	Undecryptable bool       `json:"undecryptable,omitempty"`
}

type Status struct {
	UserID   id.UserID   `json:"user_id"`
	DeviceID id.DeviceID `json:"device_id"`
	Verified bool        `json:"verified"`
	// CrossSigning is false when the account has no cross-signing keys at
	// all (it never set up a recovery key in another client).
	CrossSigning bool `json:"cross_signing"`
	Synced       bool `json:"synced"`
}
