NAME        = infra-monitor
VERSION    ?= $(shell git describe --tags --dirty 2>/dev/null || echo 0.0.0-dev)
PKG_VERSION = $(shell echo $(VERSION) | sed 's/^v//; s/-/~/g')
GOARCH     ?= $(shell go env GOARCH)
MAINTAINER  = Oche <gideon.david2015@gmail.com>
DESCRIPTION = Self-hosted observability stack (Prometheus, Grafana, Loki, Jaeger) for Docker Compose and Kubernetes.
BIN         = bin/$(NAME)
PACKAGE_DIR = $(NAME)-pkg
DISTRO      = $(shell . /etc/os-release && echo $$ID)

.PHONY: all build test vet deb rpm install clean

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN) ./cmd/$(NAME)

test: vet
	go test ./...

vet:
	go vet ./...

# Packages contain only the binary; the stack definitions are embedded in it.
deb: build
	rm -rf $(PACKAGE_DIR)
	mkdir -p $(PACKAGE_DIR)/DEBIAN $(PACKAGE_DIR)/usr/bin
	install -m 0755 $(BIN) $(PACKAGE_DIR)/usr/bin/$(NAME)
	printf 'Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: %s\nRecommends: docker.io | docker-ce, kubectl\nDescription: %s\n' \
		"$(NAME)" "$(PKG_VERSION)" "$(GOARCH)" "$(MAINTAINER)" "$(DESCRIPTION)" > $(PACKAGE_DIR)/DEBIAN/control
	dpkg-deb --root-owner-group --build $(PACKAGE_DIR) $(NAME)_$(PKG_VERSION)_$(GOARCH).deb

rpm: build
	@command -v fpm >/dev/null 2>&1 || { echo "fpm not installed. Use: gem install fpm"; exit 1; }
	fpm -s dir -t rpm -n $(NAME) -v $(PKG_VERSION) -a native \
		--maintainer "$(MAINTAINER)" --description "$(DESCRIPTION)" \
		--package $(NAME)-$(PKG_VERSION).rpm \
		$(BIN)=/usr/bin/$(NAME)

install:
	@echo "Detecting distro: $(DISTRO)..."
	@case "$(DISTRO)" in \
		debian|ubuntu) $(MAKE) deb && sudo dpkg -i $(NAME)_$(PKG_VERSION)_$(GOARCH).deb ;; \
		fedora|rhel|centos|rocky|almalinux) $(MAKE) rpm && sudo rpm -U $(NAME)-$(PKG_VERSION).rpm ;; \
		*) $(MAKE) build && sudo install -m 0755 $(BIN) /usr/local/bin/$(NAME) ;; \
	esac
	@echo "Installed $(NAME)."

clean:
	rm -rf bin $(PACKAGE_DIR) *.deb *.rpm
