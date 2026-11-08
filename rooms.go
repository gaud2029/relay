package main

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"sync"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

// Room is the little the daemon remembers about a joined room between
// restarts. Message bodies are never written to disk: history is fetched
// from the server and decrypted on demand.
type Room struct {
	ID          id.RoomID   `json:"id"`
	Name        string      `json:"name,omitempty"`
	Alias       string      `json:"alias,omitempty"`
	Heroes      []id.UserID `json:"heroes,omitempty"`
	Encrypted   bool        `json:"encrypted"`
	Unread      int         `json:"unread"`
	Highlights  int         `json:"highlights"`
	LastTS      int64       `json:"last_ts"`
	LastEventID id.EventID  `json:"last_event_id,omitempty"`
}

// RoomInfo is a Room as clients see it, with its display name worked out.
type RoomInfo struct {
	ID          id.RoomID `json:"id"`
	DisplayName string    `json:"name"`
	Encrypted   bool      `json:"encrypted"`
	Unread      int       `json:"unread"`
	Highlights  int       `json:"highlights"`
	LastTS      int64     `json:"last_ts"`
}

type RoomStore struct {
	mu    sync.Mutex
	rooms map[id.RoomID]*Room
	dirty bool
}

func loadRooms() *RoomStore {
	rs := &RoomStore{rooms: map[id.RoomID]*Room{}}
	if data, err := os.ReadFile(roomsPath()); err == nil {
		_ = json.Unmarshal(data, &rs.rooms)
	}
	return rs
}

func (rs *RoomStore) save() error {
	rs.mu.Lock()
	if !rs.dirty {
		rs.mu.Unlock()
		return nil
	}
	data, err := json.Marshal(rs.rooms)
	rs.dirty = false
	rs.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := roomsPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, roomsPath())
}

// update runs fn on the room (created if new) under the store's lock.
func (rs *RoomStore) update(roomID id.RoomID, fn func(*Room)) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	r := rs.rooms[roomID]
	if r == nil {
		r = &Room{ID: roomID}
		rs.rooms[roomID] = r
	}
	fn(r)
	rs.dirty = true
}

func (rs *RoomStore) remove(roomID id.RoomID) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if _, ok := rs.rooms[roomID]; ok {
		delete(rs.rooms, roomID)
		rs.dirty = true
	}
}

func (rs *RoomStore) get(roomID id.RoomID) (Room, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	r, ok := rs.rooms[roomID]
	if !ok {
		return Room{}, false
	}
	return *r, true
}

func (rs *RoomStore) all() []Room {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make([]Room, 0, len(rs.rooms))
	for _, r := range rs.rooms {
		out = append(out, *r)
	}
	return out
}

// displayName follows the spec's order: name, canonical alias, then the
// members ("heroes") for rooms without either, which is most DMs.
func displayName(ctx context.Context, cli *mautrix.Client, r Room) string {
	if r.Name != "" {
		return r.Name
	}
	if r.Alias != "" {
		return r.Alias
	}
	heroes := r.Heroes
	if len(heroes) == 0 && cli.StateStore != nil {
		members, _ := cli.StateStore.GetRoomJoinedOrInvitedMembers(ctx, r.ID)
		for _, m := range members {
			if m != cli.UserID {
				heroes = append(heroes, m)
			}
		}
		sort.Slice(heroes, func(i, j int) bool { return heroes[i] < heroes[j] })
		if len(heroes) > 5 {
			heroes = heroes[:5]
		}
	}
	if len(heroes) == 0 {
		return "Salon vide"
	}
	names := make([]string, len(heroes))
	for i, h := range heroes {
		names[i] = memberName(ctx, cli, r.ID, h)
	}
	return strings.Join(names, ", ")
}

func memberName(ctx context.Context, cli *mautrix.Client, roomID id.RoomID, userID id.UserID) string {
	if cli.StateStore != nil {
		if m, err := cli.StateStore.GetMember(ctx, roomID, userID); err == nil && m != nil && m.Displayname != "" {
			return m.Displayname
		}
	}
	return userID.Localpart()
}
