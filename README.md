# relay

Un client Matrix minimal, chiffrement de bout en bout compris : un démon
qui garde la session synchronisée, et une ligne de commande (ou n'importe
quelle interface, comme la barre muthur) qui lui parle sur un socket Unix.

Construit sur [mautrix-go](https://github.com/mautrix/go), avec son
implémentation d'Olm/Megolm en Go pur (`-tags goolm`) : pas de libolm.

## Installation

    make install                 # ~/.local/bin/relay + service systemd utilisateur
    relay login                  # identifiant, mot de passe, clé de récupération
    systemctl --user enable --now relay

## Utilisation

    relay status                 # compte, vérification, synchro
    relay rooms                  # salons, les plus récents d'abord
    relay chat alice             # conversation (nom partiel accepté)
    relay send "Mon salon" salut
    relay tail                   # tout ce qui arrive
    relay --json tail            # le même flux, en JSON

## Vérification

`relay login` demande la clé de récupération d'Element (Paramètres >
Sécurité). Avec elle, relay signe sa session (les autres appareils lui
font confiance et lui envoient les clés des nouveaux messages) et restaure
la sauvegarde des clés (l'historique se déchiffre). Plus tard :
`relay verify`.

## Données

- `~/.local/share/relay/` : `session.json` (jeton d'accès), `relay.db`
  (clés de chiffrement, chiffrées au repos), `rooms.json` (noms des salons,
  compteurs). Aucun message n'est écrit sur le disque.
- `$XDG_RUNTIME_DIR/relay.sock` : le socket, JSON une ligne par message.
  Le protocole est décrit dans `protocol.go`.
