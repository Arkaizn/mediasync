# mediasync — build, test and install.
PREFIX     ?= $(HOME)/.local
BINDIR     ?= $(PREFIX)/bin
UNITDIR    ?= $(HOME)/.config/systemd/user
CONFIGDIR  ?= $(if $(XDG_CONFIG_HOME),$(XDG_CONFIG_HOME),$(HOME)/.config)/mediasync
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
GO         ?= go
LDFLAGS    := -s -w -X main.version=$(VERSION)

.PHONY: all build test test-race test-integration lint fmt install uninstall clean

all: build

build:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/ ./cmd/...

test:
	$(GO) test ./...

test-race:
	$(GO) test -race -count=1 ./...

# Requires mpv + mpv-mpris and/or MEDIASYNC_IT_JELLYFIN_* (see docs/TESTING.md).
test-integration:
	$(GO) test -tags integration -count=1 -v ./test/integration/

lint:
	$(GO) vet ./...
	$(GO) vet -tags integration ./test/...
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	@if command -v golangci-lint >/dev/null; then golangci-lint run ./... && golangci-lint run --build-tags integration ./test/...; \
	else echo "golangci-lint not installed; skipped"; fi

fmt:
	gofmt -w .

install: build
	install -d "$(BINDIR)" "$(UNITDIR)"
	install -m 0755 bin/mediasyncd bin/mediasyncctl "$(BINDIR)/"
	sed 's|@BINDIR@|$(BINDIR)|g' contrib/systemd/mediasyncd.service.in > "$(UNITDIR)/mediasyncd.service"
	chmod 0644 "$(UNITDIR)/mediasyncd.service"
	@if [ ! -e "$(CONFIGDIR)/config.toml" ]; then \
		install -d -m 0700 "$(CONFIGDIR)" && \
		install -m 0600 examples/config.toml "$(CONFIGDIR)/config.toml" && \
		echo "installed example configuration to $(CONFIGDIR)/config.toml"; \
	fi
	@echo "Now run: systemctl --user daemon-reload && systemctl --user enable --now mediasyncd"

uninstall:
	-systemctl --user disable --now mediasyncd 2>/dev/null
	rm -f "$(BINDIR)/mediasyncd" "$(BINDIR)/mediasyncctl" "$(UNITDIR)/mediasyncd.service"
	-systemctl --user daemon-reload 2>/dev/null
	@echo "Configuration, state and stored credentials were kept. See README.md, 'Removal', to delete them."

clean:
	rm -rf bin
