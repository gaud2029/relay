# relay

A minimal Matrix client, end-to-end encryption included: a daemon that
keeps the session in sync, and a command line (or any interface, such as
the muthur bar) that talks to it over a Unix socket.

Built on [mautrix-go](https://github.com/mautrix/go), with its pure-Go
Olm/Megolm implementation (`-tags goolm`): no libolm needed.

## Install

Needs Go.

    make install                 # ~/.local/bin/relay + a systemd user service
    relay login                  # log in through the browser, then the recovery key
    systemctl --user enable --now relay

## Usage

    relay status                 # account, verification, sync
    relay rooms                  # rooms, most recent first
    relay chat alice             # a conversation (part of a name is enough)
    relay send "My room" hello
    relay tail                   # everything that comes in
    relay --json tail            # the same stream, as JSON

## Verification

`relay login` opens the homeserver's login page in the browser (Google,
GitHub… as in Element; `relay login --password` for an account with a
password), then asks for Element's recovery key (Settings > Security).
With it, relay signs its session (your other devices trust it and send it
the keys of new messages) and restores the key backup (the history
decrypts). Later on: `relay verify`.

## Data

- `~/.local/share/relay/`: `session.json` (access token), `relay.db`
  (encryption keys, encrypted at rest), `rooms.json` (room names,
  counters). No message is ever written to disk.
- `$XDG_RUNTIME_DIR/relay.sock`: the socket, one JSON object per line.
  The protocol is described in `protocol.go`.
