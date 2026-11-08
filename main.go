// relay is a small Matrix client: a daemon that keeps an end-to-end
// encrypted session in sync, and a command line (and any UI) that talks to
// it over a Unix socket. See protocol.go for the socket's protocol.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

const usage = `relay — client Matrix minimal

  relay login [SERVEUR]        se connecter (navigateur/SSO, ou --password)
  relay logout                 se déconnecter et effacer les données locales
  relay daemon [--notify]      lancer le démon (--notify : notifications de bureau)

  relay status                 compte, vérification, synchro
  relay rooms                  salons, les plus récents d'abord
  relay history SALON [N]      N derniers messages (30 par défaut)
  relay send SALON [TEXTE|-]   envoyer un message (- ou rien : lu sur l'entrée)
  relay chat SALON             conversation interactive
  relay tail [SALON]           afficher les messages qui arrivent
  relay verify                 vérifier avec la clé de récupération

SALON : un nom (ou un morceau de nom), un alias #salon:serveur ou un ID !…
--json (avant la commande) : sortie JSON brute, pour les scripts.
`

func main() {
	// Keys, tokens and the socket are for this user only.
	syscall.Umask(0o077)
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.BoolVar(&asJSON, "json", false, "")
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(args[0], args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "relay :", err)
		os.Exit(1)
	}
}

func run(cmd string, args []string) error {
	need := func(n int) error {
		if len(args) < n {
			flag.Usage()
			os.Exit(2)
		}
		return nil
	}
	switch cmd {
	case "login":
		return cmdLogin(args)
	case "logout":
		return cmdLogout()
	case "daemon":
		fs := flag.NewFlagSet("daemon", flag.ExitOnError)
		notify := fs.Bool("notify", false, "notifications de bureau")
		debug := fs.Bool("debug", false, "journal détaillé")
		_ = fs.Parse(args)
		return runDaemon(*notify, *debug)
	case "status":
		return cmdStatus()
	case "rooms":
		return cmdRooms()
	case "history":
		_ = need(1)
		limit := 30
		if len(args) > 1 {
			n, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("nombre de messages invalide %q", args[1])
			}
			limit = n
		}
		return cmdHistory(args[0], limit)
	case "send":
		_ = need(1)
		return cmdSend(args[0], strings.Join(args[1:], " "))
	case "chat":
		_ = need(1)
		return cmdChat(args[0])
	case "tail":
		room := ""
		if len(args) > 0 {
			room = args[0]
		}
		return cmdTail(room)
	case "verify":
		return cmdVerify()
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		return fmt.Errorf("commande inconnue %q (relay help)", cmd)
	}
}
