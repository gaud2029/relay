package main

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"maunium.net/go/mautrix/id"
)

// A client that shows a room silences that room's notifications, until it
// shows another one, none, or disconnects.
func TestFocusSilencesShownRoom(t *testing.T) {
	d := &Daemon{rooms: &RoomStore{rooms: map[id.RoomID]*Room{}}, subs: map[*conn]struct{}{}, conns: map[*conn]struct{}{}}
	ln, err := net.Listen("unix", filepath.Join(t.TempDir(), "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go d.serve(ln)

	c, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(c)
	ask := func(room string) {
		t.Helper()
		data, _ := json.Marshal(Request{ID: 1, Cmd: "focus", Room: room})
		_, _ = c.Write(append(data, '\n'))
		if !sc.Scan() {
			t.Fatal("no reply")
		}
		var r Reply
		_ = json.Unmarshal(sc.Bytes(), &r)
		if !r.OK {
			t.Fatalf("focus %q: %s", room, r.Error)
		}
	}
	a, b := id.RoomID("!a:x"), id.RoomID("!b:x")

	if d.shown(a) {
		t.Fatal("nothing shown yet")
	}
	ask(string(a))
	if !d.shown(a) || d.shown(b) {
		t.Fatal("only !a should be shown")
	}
	ask(string(b))
	if d.shown(a) || !d.shown(b) {
		t.Fatal("focus should have moved to !b")
	}
	ask("")
	if d.shown(b) {
		t.Fatal("no room should be shown")
	}
	ask(string(a))
	c.Close()
	deadline := time.Now().Add(2 * time.Second)
	for d.shown(a) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if d.shown(a) {
		t.Fatal("a disconnected client still silences !a")
	}
}
