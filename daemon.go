package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type Daemon struct {
	cli    *mautrix.Client
	crypto *cryptohelper.CryptoHelper
	rooms  *RoomStore
	log    zerolog.Logger
	notify bool

	mu       sync.Mutex
	subs     map[*conn]struct{}
	synced   bool
	verified bool
}

type conn struct {
	c  net.Conn
	mu sync.Mutex
}

func (c *conn) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err = c.c.Write(append(data, '\n'))
	return err
}

func runDaemon(notify bool, debug bool) error {
	level := zerolog.InfoLevel
	if debug {
		level = zerolog.DebugLevel
	}
	log := newLogger(level)
	s, err := loadSession()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d := &Daemon{rooms: loadRooms(), log: log, notify: notify, subs: map[*conn]struct{}{}}
	ln, err := listen()
	if err != nil {
		return err
	}
	defer os.Remove(socketPath())
	defer ln.Close()

	cli, helper, err := openClient(ctx, s, log)
	if err != nil {
		return err
	}
	defer helper.Close()
	d.cli, d.crypto = cli, helper
	d.hookSync()
	d.refreshVerified(ctx)

	go d.serve(ln)
	go d.saveLoop(ctx)

	log.Info().Str("user", s.UserID.String()).Str("device", s.DeviceID.String()).
		Str("socket", socketPath()).Msg("relay démarré")
	err = cli.SyncWithContext(ctx)
	_ = d.rooms.save()
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// listen opens the socket, refusing to run twice: a socket that still
// answers belongs to a live daemon, one that doesn't is a leftover.
func listen() (net.Listener, error) {
	path := socketPath()
	if c, err := net.Dial("unix", path); err == nil {
		c.Close()
		return nil, fmt.Errorf("un démon relay tourne déjà (%s)", path)
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return ln, nil
}

func (d *Daemon) saveLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := d.rooms.save(); err != nil {
				d.log.Warn().Err(err).Msg("sauvegarde des salons")
			}
		}
	}
}

// live is false while the daemon replays the first sync after a fresh
// start without a sync token: those messages are old news.
func live(ctx context.Context) bool {
	since, _ := ctx.Value(mautrix.SyncTokenContextKey).(string)
	return since != ""
}

func (d *Daemon) hookSync() {
	syncer := d.cli.Syncer.(*mautrix.DefaultSyncer)

	syncer.OnSync(func(ctx context.Context, resp *mautrix.RespSync, since string) bool {
		changed := false
		for roomID, room := range resp.Rooms.Join {
			d.rooms.update(roomID, func(r *Room) {
				if len(room.Summary.Heroes) > 0 {
					r.Heroes = without(room.Summary.Heroes, d.cli.UserID)
				}
				if n := room.UnreadNotifications; n != nil {
					r.Unread, r.Highlights = n.NotificationCount, n.HighlightCount
				}
			})
			changed = true
		}
		for roomID := range resp.Rooms.Leave {
			d.rooms.remove(roomID)
			changed = true
		}
		if changed && since != "" {
			// After the events of this sync have been handled.
			go func() { d.push("rooms", d.roomList(context.Background())) }()
		}
		return true
	})
	syncer.OnSync(func(ctx context.Context, resp *mautrix.RespSync, since string) bool {
		d.mu.Lock()
		first := !d.synced
		d.synced = true
		d.mu.Unlock()
		if first {
			go d.pushStatus(context.Background())
		}
		return true
	})

	syncer.OnEventType(event.StateRoomName, func(ctx context.Context, evt *event.Event) {
		d.rooms.update(evt.RoomID, func(r *Room) { r.Name = evt.Content.AsRoomName().Name })
	})
	syncer.OnEventType(event.StateCanonicalAlias, func(ctx context.Context, evt *event.Event) {
		d.rooms.update(evt.RoomID, func(r *Room) { r.Alias = evt.Content.AsCanonicalAlias().Alias.String() })
	})
	syncer.OnEventType(event.StateEncryption, func(ctx context.Context, evt *event.Event) {
		d.rooms.update(evt.RoomID, func(r *Room) { r.Encrypted = true })
	})
	syncer.OnEventType(event.EventMessage, func(ctx context.Context, evt *event.Event) {
		d.rooms.update(evt.RoomID, func(r *Room) {
			if evt.Timestamp >= r.LastTS {
				r.LastTS, r.LastEventID = evt.Timestamp, evt.ID
			}
		})
		if !live(ctx) {
			return
		}
		msg := d.toMessage(ctx, evt)
		d.push("message", msg)
		d.notifyDesktop(msg)
	})
	d.crypto.DecryptErrorCallback = func(evt *event.Event, err error) {
		d.rooms.update(evt.RoomID, func(r *Room) {
			if evt.Timestamp >= r.LastTS {
				r.LastTS, r.LastEventID = evt.Timestamp, evt.ID
			}
		})
		d.log.Warn().Err(err).Str("room", evt.RoomID.String()).Str("event", evt.ID.String()).Msg("message indéchiffrable")
		d.push("message", d.undecryptable(context.Background(), evt))
	}
}

func without(users []id.UserID, self id.UserID) []id.UserID {
	out := make([]id.UserID, 0, len(users))
	for _, u := range users {
		if u != self {
			out = append(out, u)
		}
	}
	return out
}

func (d *Daemon) roomName(ctx context.Context, roomID id.RoomID) string {
	r, ok := d.rooms.get(roomID)
	if !ok {
		r = Room{ID: roomID}
	}
	return displayName(ctx, d.cli, r)
}

func (d *Daemon) toMessage(ctx context.Context, evt *event.Event) Message {
	content := evt.Content.AsMessage()
	m := Message{
		ID:         evt.ID,
		Room:       evt.RoomID,
		RoomName:   d.roomName(ctx, evt.RoomID),
		Sender:     evt.Sender,
		SenderName: memberName(ctx, d.cli, evt.RoomID, evt.Sender),
		TS:         evt.Timestamp,
		MsgType:    string(content.MsgType),
		Body:       content.Body,
		Own:        evt.Sender == d.cli.UserID,
		Encrypted:  evt.Mautrix.WasEncrypted,
	}
	if content.RelatesTo.GetReplaceID() != "" && content.NewContent != nil {
		m.Body, m.MsgType, m.Edit = content.NewContent.Body, string(content.NewContent.MsgType), true
	}
	if content.MsgType == event.MsgImage || content.MsgType == event.MsgFile ||
		content.MsgType == event.MsgVideo || content.MsgType == event.MsgAudio {
		m.Body = "[" + strings.TrimPrefix(string(content.MsgType), "m.") + "] " + content.Body
	}
	return m
}

func (d *Daemon) undecryptable(ctx context.Context, evt *event.Event) Message {
	return Message{
		ID:            evt.ID,
		Room:          evt.RoomID,
		RoomName:      d.roomName(ctx, evt.RoomID),
		Sender:        evt.Sender,
		SenderName:    memberName(ctx, d.cli, evt.RoomID, evt.Sender),
		TS:            evt.Timestamp,
		Own:           evt.Sender == d.cli.UserID,
		Encrypted:     true,
		Undecryptable: true,
	}
}

func (d *Daemon) notifyDesktop(m Message) {
	if !d.notify || m.Own || m.Edit {
		return
	}
	title := m.SenderName
	if m.RoomName != m.SenderName {
		title += " · " + m.RoomName
	}
	_ = exec.Command("notify-send", "--app-name=relay", "--icon=mail-message-new", title, m.Body).Start()
}

func (d *Daemon) push(name string, data any) {
	d.mu.Lock()
	subs := make([]*conn, 0, len(d.subs))
	for c := range d.subs {
		subs = append(subs, c)
	}
	d.mu.Unlock()
	for _, c := range subs {
		if err := c.write(Push{Event: name, Data: data}); err != nil {
			d.unsubscribe(c)
		}
	}
}

func (d *Daemon) unsubscribe(c *conn) {
	d.mu.Lock()
	delete(d.subs, c)
	d.mu.Unlock()
}

func (d *Daemon) refreshVerified(ctx context.Context) (hasKeys bool) {
	hasKeys, verified, err := d.crypto.Machine().GetOwnVerificationStatus(ctx)
	if err != nil {
		d.log.Warn().Err(err).Msg("état de vérification")
	}
	d.mu.Lock()
	d.verified = verified
	d.mu.Unlock()
	return hasKeys
}

func (d *Daemon) status(ctx context.Context) Status {
	hasKeys := d.refreshVerified(ctx)
	d.mu.Lock()
	defer d.mu.Unlock()
	return Status{
		UserID: d.cli.UserID, DeviceID: d.cli.DeviceID,
		Verified: d.verified, CrossSigning: hasKeys, Synced: d.synced,
	}
}

func (d *Daemon) pushStatus(ctx context.Context) { d.push("status", d.status(ctx)) }

func (d *Daemon) roomList(ctx context.Context) []RoomInfo {
	rooms := d.rooms.all()
	sort.Slice(rooms, func(i, j int) bool { return rooms[i].LastTS > rooms[j].LastTS })
	out := make([]RoomInfo, len(rooms))
	for i, r := range rooms {
		out[i] = RoomInfo{
			ID: r.ID, DisplayName: displayName(ctx, d.cli, r), Encrypted: r.Encrypted,
			Unread: r.Unread, Highlights: r.Highlights, LastTS: r.LastTS,
		}
	}
	return out
}

// resolveRoom accepts a room ID, an alias, a room's exact name, or a piece
// of a name that only one room has.
func (d *Daemon) resolveRoom(ctx context.Context, q string) (id.RoomID, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return "", errors.New("salon manquant")
	}
	rooms := d.roomList(ctx)
	if strings.HasPrefix(q, "!") {
		return id.RoomID(q), nil
	}
	if strings.HasPrefix(q, "#") {
		for _, r := range d.rooms.all() {
			if r.Alias == q {
				return r.ID, nil
			}
		}
		resp, err := d.cli.ResolveAlias(ctx, id.RoomAlias(q))
		if err != nil {
			return "", fmt.Errorf("alias %s : %w", q, err)
		}
		return resp.RoomID, nil
	}
	lq := strings.ToLower(q)
	var partial []RoomInfo
	for _, r := range rooms {
		name := strings.ToLower(r.DisplayName)
		if name == lq {
			return r.ID, nil
		}
		if strings.Contains(name, lq) {
			partial = append(partial, r)
		}
	}
	switch len(partial) {
	case 1:
		return partial[0].ID, nil
	case 0:
		return "", fmt.Errorf("aucun salon ne correspond à %q", q)
	default:
		names := make([]string, len(partial))
		for i, r := range partial {
			names[i] = r.DisplayName
		}
		return "", fmt.Errorf("%q est ambigu : %s", q, strings.Join(names, " / "))
	}
}

func (d *Daemon) history(ctx context.Context, roomID id.RoomID, limit int) ([]Message, error) {
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	resp, err := d.cli.Messages(ctx, roomID, "", "", mautrix.DirectionBackward, nil, limit*2)
	if err != nil {
		return nil, err
	}
	var out []Message
	for _, evt := range resp.Chunk {
		evt.RoomID = roomID
		if evt.Type == event.EventEncrypted {
			_ = evt.Content.ParseRaw(evt.Type)
			dec, err := d.crypto.Decrypt(ctx, evt)
			if err != nil {
				out = append(out, d.undecryptable(ctx, evt))
				continue
			}
			evt = dec
		} else {
			_ = evt.Content.ParseRaw(evt.Type)
		}
		if evt.Type != event.EventMessage {
			continue
		}
		if c := evt.Content.AsMessage(); c.RelatesTo.GetReplaceID() != "" {
			continue // edits: the history shows the original message
		}
		out = append(out, d.toMessage(ctx, evt))
		if len(out) == limit {
			break
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (d *Daemon) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go d.handleConn(&conn{c: c})
	}
}

func (d *Daemon) handleConn(c *conn) {
	defer c.c.Close()
	defer d.unsubscribe(c)
	sc := bufio.NewScanner(c.c)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var req Request
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			_ = c.write(Reply{OK: false, Error: "requête JSON invalide"})
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		data, err := d.handle(ctx, c, &req)
		cancel()
		reply := Reply{ID: req.ID, OK: err == nil, Data: data}
		if err != nil {
			reply.Error = err.Error()
		}
		if c.write(reply) != nil {
			return
		}
	}
}

func (d *Daemon) handle(ctx context.Context, c *conn, req *Request) (any, error) {
	switch req.Cmd {
	case "status":
		return d.status(ctx), nil
	case "rooms":
		return d.roomList(ctx), nil
	case "subscribe":
		d.mu.Lock()
		d.subs[c] = struct{}{}
		d.mu.Unlock()
		return nil, nil
	case "resolve":
		roomID, err := d.resolveRoom(ctx, req.Room)
		if err != nil {
			return nil, err
		}
		return map[string]any{"room": roomID, "name": d.roomName(ctx, roomID)}, nil
	case "history":
		roomID, err := d.resolveRoom(ctx, req.Room)
		if err != nil {
			return nil, err
		}
		return d.history(ctx, roomID, req.Limit)
	case "send":
		roomID, err := d.resolveRoom(ctx, req.Room)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(req.Body) == "" {
			return nil, errors.New("message vide")
		}
		resp, err := d.cli.SendText(ctx, roomID, req.Body)
		if err != nil {
			return nil, err
		}
		return map[string]any{"event_id": resp.EventID, "room": roomID}, nil
	case "read":
		roomID, err := d.resolveRoom(ctx, req.Room)
		if err != nil {
			return nil, err
		}
		r, _ := d.rooms.get(roomID)
		if r.LastEventID == "" {
			return nil, nil
		}
		if err := d.cli.MarkRead(ctx, roomID, r.LastEventID); err != nil {
			return nil, err
		}
		d.rooms.update(roomID, func(r *Room) { r.Unread, r.Highlights = 0, 0 })
		go d.push("rooms", d.roomList(context.Background()))
		return nil, nil
	case "verify":
		count, err := verifyWithRecoveryKey(ctx, d.crypto.Machine(), req.RecoveryKey)
		go d.pushStatus(context.Background())
		if err != nil {
			return nil, err
		}
		return map[string]any{"restored_keys": count}, nil
	default:
		return nil, fmt.Errorf("commande inconnue %q", req.Cmd)
	}
}
