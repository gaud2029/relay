package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/term"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

// client is one connection to the daemon.
type client struct {
	c      net.Conn
	sc     *bufio.Scanner
	mu     sync.Mutex
	nextID int64
}

func dial() (*client, error) {
	c, err := net.Dial("unix", socketPath())
	if err != nil {
		return nil, fmt.Errorf("le démon ne répond pas sur %s (lance `relay daemon`)", socketPath())
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	return &client{c: c, sc: sc}, nil
}

func (cl *client) send(req Request) (int64, error) {
	cl.mu.Lock()
	cl.nextID++
	req.ID = cl.nextID
	cl.mu.Unlock()
	data, _ := json.Marshal(req)
	_, err := cl.c.Write(append(data, '\n'))
	return req.ID, err
}

func (cl *client) next() (*Incoming, error) {
	if !cl.sc.Scan() {
		if err := cl.sc.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("le démon a fermé la connexion")
	}
	var in Incoming
	if err := json.Unmarshal(cl.sc.Bytes(), &in); err != nil {
		return nil, err
	}
	return &in, nil
}

// call sends a request and waits for its reply, decoding its data into out.
// Events that arrive meanwhile are dropped; use it before subscribing.
func (cl *client) call(req Request, out any) error {
	reqID, err := cl.send(req)
	if err != nil {
		return err
	}
	for {
		in, err := cl.next()
		if err != nil {
			return err
		}
		if in.Event != "" || in.ID != reqID {
			continue
		}
		if !in.OK {
			return errors.New(in.Error)
		}
		if out != nil && len(in.Data) > 0 {
			return json.Unmarshal(in.Data, out)
		}
		return nil
	}
}

func call(req Request, out any) error {
	cl, err := dial()
	if err != nil {
		return err
	}
	defer cl.c.Close()
	return cl.call(req, out)
}

// --- output ---

var asJSON bool

func printJSON(v any) {
	data, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(data))
}

func formatTS(ts int64) string {
	t := time.UnixMilli(ts)
	if time.Since(t) < 20*time.Hour && t.Day() == time.Now().Day() {
		return t.Format("15:04")
	}
	return t.Format("02/01 15:04")
}

func formatMessage(m Message, withRoom bool) string {
	var b strings.Builder
	b.WriteString("[" + formatTS(m.TS) + "] ")
	if withRoom && m.RoomName != m.SenderName {
		b.WriteString(m.RoomName + " · ")
	}
	b.WriteString(m.SenderName)
	switch {
	case m.Undecryptable:
		b.WriteString(" : 🔒 message indéchiffrable (clés manquantes)")
	case m.MsgType == "m.emote":
		b.WriteString(" " + m.Body)
	default:
		b.WriteString(" : " + m.Body)
	}
	if m.Edit {
		b.WriteString(" (modifié)")
	}
	return b.String()
}

// --- commands ---

func cmdLogin(args []string) error {
	if _, err := loadSession(); err == nil {
		return errors.New("déjà connecté (relay logout pour changer de compte)")
	}
	usePassword := false
	account := ""
	for _, a := range args {
		if a == "--password" {
			usePassword = true
		} else {
			account = a
		}
	}
	in := bufio.NewReader(os.Stdin)
	if account == "" {
		fmt.Print("Serveur ou identifiant Matrix [matrix.org] : ")
		line, _ := in.ReadString('\n')
		if account = strings.TrimSpace(line); account == "" {
			account = "matrix.org"
		}
	}
	server, err := serverName(account)
	if err != nil {
		return err
	}
	ctx := context.Background()
	log := newLogger(zerolog.WarnLevel)
	cli, err := loginClient(ctx, server, log)
	if err != nil {
		return err
	}
	flows, err := cli.GetLoginFlows(ctx)
	if err != nil {
		return fmt.Errorf("%s ne répond pas comme un serveur Matrix : %w", cli.HomeserverURL, err)
	}

	var req *mautrix.ReqLogin
	switch {
	case flows.HasFlow(mautrix.AuthTypeSSO) && !usePassword:
		// Google, GitHub, Apple... and on matrix.org, the account page
		// with its password, all live behind the server's SSO page.
		token, err := ssoLoginToken(ctx, cli)
		if err != nil {
			return err
		}
		req = &mautrix.ReqLogin{Type: mautrix.AuthTypeToken, Token: token}
	case flows.HasFlow(mautrix.AuthTypePassword):
		user := account
		if !strings.HasPrefix(user, "@") {
			fmt.Print("Nom d'utilisateur : ")
			line, _ := in.ReadString('\n')
			user = "@" + strings.TrimPrefix(strings.TrimSpace(line), "@") + ":" + server
		}
		password, err := readSecret("Mot de passe : ")
		if err != nil {
			return err
		}
		req = &mautrix.ReqLogin{
			Type:       mautrix.AuthTypePassword,
			Identifier: mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: user},
			Password:   password,
		}
	default:
		return fmt.Errorf("%s n'accepte ni SSO ni mot de passe", server)
	}

	fmt.Println("Connexion…")
	helper, err := login(ctx, cli, req)
	if err != nil {
		_ = os.Remove(dbPath())
		return fmt.Errorf("connexion refusée : %w", err)
	}
	defer helper.Close()
	s, _ := loadSession()
	fmt.Printf("Connecté : %s, appareil %s\n\n", s.UserID, s.DeviceID)

	fmt.Println("Pour lire tes messages chiffrés, cette session doit être vérifiée.")
	fmt.Println("Entre ta clé de récupération (Element : Paramètres > Sécurité > Sauvegarde")
	fmt.Println("et récupération), ou laisse vide pour le faire plus tard avec `relay verify`.")
	key, err := readSecret("Clé de récupération : ")
	if err != nil || strings.TrimSpace(key) == "" {
		fmt.Println("Vérification remise à plus tard.")
		return nil
	}
	fmt.Println("Vérification et restauration des clés…")
	count, err := verifyWithRecoveryKey(ctx, helper.Machine(), key)
	if err != nil {
		return err
	}
	fmt.Printf("Session vérifiée, %d clés de salon restaurées.\n", count)
	return nil
}

func readSecret(prompt string) (string, error) {
	fmt.Print(prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		return string(b), err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line), err
}

func cmdLogout() error {
	s, err := loadSession()
	if err != nil {
		return err
	}
	if c, err := net.Dial("unix", socketPath()); err == nil {
		c.Close()
		return errors.New("arrête le démon d'abord (systemctl --user stop relay)")
	}
	cli, helper, err := openClient(context.Background(), s, newLogger(zerolog.WarnLevel))
	if err == nil {
		_, err = cli.Logout(context.Background())
		helper.Close()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "déconnexion côté serveur impossible :", err)
	}
	for _, p := range []string{sessionPath(), dbPath(), dbPath() + "-wal", dbPath() + "-shm", roomsPath()} {
		_ = os.Remove(p)
	}
	fmt.Println("Déconnecté, données locales effacées.")
	return nil
}

func cmdStatus() error {
	var st Status
	if err := call(Request{Cmd: "status"}, &st); err != nil {
		return err
	}
	if asJSON {
		printJSON(st)
		return nil
	}
	fmt.Printf("Compte      %s\nAppareil    %s\n", st.UserID, st.DeviceID)
	switch {
	case st.Verified:
		fmt.Println("Session     vérifiée")
	case !st.CrossSigning:
		fmt.Println("Session     non vérifiée (le compte n'a pas de clé de récupération)")
	default:
		fmt.Println("Session     NON vérifiée : relay verify")
	}
	if st.Synced {
		fmt.Println("Synchro     à jour")
	} else {
		fmt.Println("Synchro     en cours…")
	}
	return nil
}

func cmdRooms() error {
	var rooms []RoomInfo
	if err := call(Request{Cmd: "rooms"}, &rooms); err != nil {
		return err
	}
	if asJSON {
		printJSON(rooms)
		return nil
	}
	for _, r := range rooms {
		unread := "   "
		if r.Unread > 0 {
			unread = fmt.Sprintf("%3d", r.Unread)
		}
		lock := " "
		if r.Encrypted {
			lock = "🔒"
		}
		fmt.Printf("%s %s %s\n", unread, lock, r.DisplayName)
	}
	return nil
}

func cmdHistory(room string, limit int) error {
	var msgs []Message
	if err := call(Request{Cmd: "history", Room: room, Limit: limit}, &msgs); err != nil {
		return err
	}
	if asJSON {
		printJSON(msgs)
		return nil
	}
	for _, m := range msgs {
		fmt.Println(formatMessage(m, false))
	}
	return nil
}

func cmdSend(room, body string) error {
	if body == "-" || body == "" {
		data, err := readAllStdin()
		if err != nil {
			return err
		}
		body = data
	}
	return call(Request{Cmd: "send", Room: room, Body: body}, nil)
}

func readAllStdin() (string, error) {
	var b strings.Builder
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(sc.Text())
	}
	return b.String(), sc.Err()
}

func cmdVerify() error {
	key, err := readSecret("Clé de récupération : ")
	if err != nil {
		return err
	}
	var res struct {
		RestoredKeys int `json:"restored_keys"`
	}
	fmt.Println("Vérification et restauration des clés…")
	if err := call(Request{Cmd: "verify", RecoveryKey: key}, &res); err != nil {
		return err
	}
	fmt.Printf("Session vérifiée, %d clés de salon restaurées.\n", res.RestoredKeys)
	return nil
}

// cmdTail prints every event as it comes; with a room, only that room's
// messages. As JSON, it's the raw feed a UI would read.
func cmdTail(room string) error {
	cl, err := dial()
	if err != nil {
		return err
	}
	defer cl.c.Close()
	if err := cl.call(Request{Cmd: "subscribe"}, nil); err != nil {
		return err
	}
	roomID := id.RoomID("")
	if room != "" {
		if roomID, err = resolveVia(cl, room); err != nil {
			return err
		}
	}
	for {
		in, err := cl.next()
		if err != nil {
			return err
		}
		if in.Event == "" {
			continue
		}
		if asJSON {
			fmt.Printf("{\"event\":%q,\"data\":%s}\n", in.Event, in.Data)
			continue
		}
		if in.Event == "message" {
			var m Message
			if json.Unmarshal(in.Data, &m) == nil && (roomID == "" || m.Room == roomID) {
				fmt.Println(formatMessage(m, roomID == ""))
			}
		}
	}
}

// resolveVia asks the daemon which room a name points at.
func resolveVia(cl *client, room string) (id.RoomID, error) {
	var res struct {
		Room id.RoomID `json:"room"`
	}
	if err := cl.call(Request{Cmd: "resolve", Room: room}, &res); err != nil {
		return "", err
	}
	return res.Room, nil
}

// cmdChat is a minimal conversation view: the last messages, then new ones
// as they arrive, and every line typed is sent.
func cmdChat(room string) error {
	cl, err := dial()
	if err != nil {
		return err
	}
	defer cl.c.Close()
	roomID, err := resolveVia(cl, room)
	if err != nil {
		return err
	}
	var hist []Message
	if err := cl.call(Request{Cmd: "history", Room: string(roomID), Limit: 20}, &hist); err != nil {
		return err
	}
	for _, m := range hist {
		fmt.Println(formatMessage(m, false))
	}
	if err := cl.call(Request{Cmd: "subscribe"}, nil); err != nil {
		return err
	}
	_, _ = cl.send(Request{Cmd: "read", Room: string(roomID)})
	fmt.Println("— Entrée pour envoyer, Ctrl+D pour quitter —")

	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				// Clear the echoed input line; the message comes back through sync.
				fmt.Print("\033[1A\033[2K")
				_, _ = cl.send(Request{Cmd: "send", Room: string(roomID), Body: line})
			}
		}
		cl.c.Close()
	}()
	for {
		in, err := cl.next()
		if err != nil {
			return nil
		}
		switch {
		case in.Event == "message":
			var m Message
			if json.Unmarshal(in.Data, &m) == nil && m.Room == roomID {
				fmt.Println(formatMessage(m, false))
				_, _ = cl.send(Request{Cmd: "read", Room: string(roomID)})
			}
		case in.Event == "" && !in.OK:
			fmt.Fprintln(os.Stderr, "erreur :", in.Error)
		}
	}
}
