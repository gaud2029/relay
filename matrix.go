package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/rs/zerolog"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/crypto/ssss"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func newLogger(level zerolog.Level) zerolog.Logger {
	return zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05"}).
		With().Timestamp().Logger().Level(level)
}

// serverName takes "@user:example.org" or "example.org" to "example.org".
func serverName(account string) (string, error) {
	account = strings.TrimSpace(account)
	if strings.HasPrefix(account, "@") {
		_, server, err := id.UserID(account).Parse()
		if err != nil || server == "" {
			return "", fmt.Errorf("identifiant invalide %q (attendu : @nom:serveur)", account)
		}
		return server, nil
	}
	if account == "" || strings.ContainsAny(account, "/ @") {
		return "", fmt.Errorf("serveur invalide %q", account)
	}
	return account, nil
}

// discoverHomeserver turns "example.org" into the client API URL that
// example.org's .well-known points at (or https://example.org).
func discoverHomeserver(ctx context.Context, server string) (string, error) {
	wk, err := mautrix.DiscoverClientAPI(ctx, server)
	if err == nil && wk != nil && wk.Homeserver.BaseURL != "" {
		return strings.TrimRight(wk.Homeserver.BaseURL, "/"), nil
	}
	return "https://" + server, nil
}

// openClient builds a logged-in client with end-to-end encryption on top of
// the stored session.
func openClient(ctx context.Context, s *Session, log zerolog.Logger) (*mautrix.Client, *cryptohelper.CryptoHelper, error) {
	cli, err := mautrix.NewClient(s.Homeserver, s.UserID, s.AccessToken)
	if err != nil {
		return nil, nil, err
	}
	cli.DeviceID = s.DeviceID
	cli.Log = log
	helper, err := newCryptoHelper(cli, s)
	if err != nil {
		return nil, nil, err
	}
	if err := helper.Init(ctx); err != nil {
		return nil, nil, fmt.Errorf("initialisation du chiffrement : %w", err)
	}
	cli.Crypto = helper
	return cli, helper, nil
}

func newCryptoHelper(cli *mautrix.Client, s *Session) (*cryptohelper.CryptoHelper, error) {
	key, err := base64.StdEncoding.DecodeString(s.PickleKey)
	if err != nil || len(key) == 0 {
		return nil, fmt.Errorf("clé de chiffrement locale invalide dans %s", sessionPath())
	}
	if err := os.MkdirAll(dataDir(), 0o700); err != nil {
		return nil, err
	}
	return cryptohelper.NewCryptoHelper(cli, key, dbPath())
}

// loginClient is an anonymous client for the homeserver of a server name.
func loginClient(ctx context.Context, server string, log zerolog.Logger) (*mautrix.Client, error) {
	hs, err := discoverHomeserver(ctx, server)
	if err != nil {
		return nil, err
	}
	cli, err := mautrix.NewClient(hs, "", "")
	if err != nil {
		return nil, err
	}
	cli.Log = log
	return cli, nil
}

// login signs in (with a password or an SSO login token) and sets up a new
// device with its own encryption keys. The returned session is already saved.
func login(ctx context.Context, cli *mautrix.Client, req *mautrix.ReqLogin) (*cryptohelper.CryptoHelper, error) {
	s := &Session{Homeserver: cli.HomeserverURL.String(), PickleKey: newPickleKey()}
	helper, err := newCryptoHelper(cli, s)
	if err != nil {
		return nil, err
	}
	hostname, _ := os.Hostname()
	req.InitialDeviceDisplayName = "relay (" + hostname + ")"
	helper.LoginAs = req
	if err := helper.Init(ctx); err != nil {
		helper.Close()
		return nil, err
	}
	cli.Crypto = helper
	s.UserID, s.DeviceID, s.AccessToken = cli.UserID, cli.DeviceID, cli.AccessToken
	if err := s.save(); err != nil {
		helper.Close()
		return nil, err
	}
	return helper, nil
}

const ssoDonePage = `<!doctype html><meta charset="utf-8"><title>relay</title>
<body style="font:16px monospace;background:#111;color:#cfc;padding:3em">
<p>Connexion transmise à relay. Tu peux fermer cet onglet.</p>`

// ssoLoginToken runs the browser half of an SSO login: it opens the
// homeserver's SSO page (where "Continue with Google" and the like live),
// which sends the browser back to a one-shot local web server with a login
// token. That token is then traded for an access token with m.login.token.
func ssoLoginToken(ctx context.Context, cli *mautrix.Client) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	redirect := fmt.Sprintf("http://%s/sso", ln.Addr())
	ssoURL := cli.BuildClientURL("v3", "login", "sso", "redirect") + "?redirectUrl=" + url.QueryEscape(redirect)

	tokens := make(chan string, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("loginToken")
		if r.URL.Path != "/sso" || token == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, ssoDonePage)
		select {
		case tokens <- token:
		default:
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	fmt.Println("Ouverture du navigateur pour la connexion…")
	fmt.Println("S'il ne s'ouvre pas, va sur cette adresse :")
	fmt.Println("  " + ssoURL)
	_ = exec.Command("xdg-open", ssoURL).Start()

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	select {
	case token := <-tokens:
		// Let the browser get its page before the server goes away.
		time.Sleep(300 * time.Millisecond)
		return token, nil
	case <-ctx.Done():
		return "", errors.New("pas de réponse du navigateur après 10 minutes")
	}
}

// verifyWithRecoveryKey does what "Verify with Security Key" does in Element:
// it unlocks secret storage with the recovery key, cross-signs this device
// (so other clients trust it and share keys with it), then restores every
// room key from the server-side backup so that old messages decrypt too.
// It returns how many room keys the backup held.
func verifyWithRecoveryKey(ctx context.Context, mach *crypto.OlmMachine, recoveryKey string) (int, error) {
	recoveryKey = strings.TrimSpace(recoveryKey)
	keyID, keyData, err := mach.SSSS.GetDefaultKeyData(ctx)
	if err != nil {
		return 0, fmt.Errorf("pas de stockage sécurisé sur ce compte : %w", err)
	}
	key, err := keyData.VerifyRecoveryKey(keyID, recoveryKey)
	if err != nil && !errors.Is(err, ssss.ErrUnverifiableKey) {
		return 0, fmt.Errorf("clé de récupération refusée : %w", err)
	}
	if err := mach.FetchCrossSigningKeysFromSSSS(ctx, key); err != nil {
		return 0, fmt.Errorf("clés de signature croisée : %w", err)
	}
	if err := mach.SignOwnDevice(ctx, mach.OwnIdentity()); err != nil {
		return 0, fmt.Errorf("signature de l'appareil : %w", err)
	}
	if err := mach.SignOwnMasterKey(ctx); err != nil {
		return 0, fmt.Errorf("signature de la clé maîtresse : %w", err)
	}

	raw, err := mach.SSSS.GetDecryptedAccountData(ctx, event.AccountDataMegolmBackupKey, key)
	if err != nil {
		return 0, fmt.Errorf("appareil vérifié, mais pas de sauvegarde des clés : %w", err)
	}
	backupKey, err := backup.MegolmBackupKeyFromBytes(raw)
	if err != nil {
		return 0, fmt.Errorf("clé de sauvegarde invalide : %w", err)
	}
	info, err := mach.GetAndVerifyLatestKeyBackupVersion(ctx, backupKey)
	if err != nil {
		return 0, fmt.Errorf("sauvegarde des clés : %w", err)
	} else if info == nil {
		return 0, nil
	}
	if err := mach.GetAndStoreKeyBackup(ctx, info.Version, backupKey); err != nil {
		return 0, fmt.Errorf("restauration de la sauvegarde : %w", err)
	}
	return info.Count, nil
}
