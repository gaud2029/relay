# goolm: mautrix-go's pure-Go Olm/Megolm, so no libolm (deprecated) is needed.
TAGS   := goolm
PREFIX ?= $(HOME)/.local

relay: $(wildcard *.go) go.mod go.sum
	go build -tags $(TAGS) -trimpath -o relay .

install: relay
	install -Dm755 relay $(PREFIX)/bin/relay
	install -Dm644 relay.service $(HOME)/.config/systemd/user/relay.service
	systemctl --user daemon-reload

vet:
	go vet -tags $(TAGS) ./...

clean:
	rm -f relay

.PHONY: install vet clean
