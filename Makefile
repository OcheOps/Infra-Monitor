NAME     = infra-monitor
VERSION ?= $(shell git describe --tags --dirty 2>/dev/null || echo 0.0.0-dev)
BIN      = bin/$(NAME)
PREFIX  ?= /usr/local

.PHONY: all build test vet snapshot install uninstall clean

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN) ./cmd/$(NAME)

test: vet
	go test ./...

vet:
	go vet ./...

# Build every release artifact (binaries, .deb, .rpm, archives) into dist/
# without publishing. Needs goreleaser: https://goreleaser.com/install/
snapshot:
	goreleaser release --snapshot --clean

install: build
	install -d $(DESTDIR)$(PREFIX)/bin
	install -m 0755 $(BIN) $(DESTDIR)$(PREFIX)/bin/$(NAME)
	@echo "Installed $(DESTDIR)$(PREFIX)/bin/$(NAME)"

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/$(NAME)

clean:
	rm -rf bin dist
