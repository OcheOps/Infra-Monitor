NAME        = infra-monitor
VERSION     = 1.0.0
ARCH        = all
PACKAGE_DIR = $(NAME)-pkg
MAINTAINER  = Oche <gideon.david2015@gmail.com>
DESCRIPTION = Self-hosted infra monitor with Prometheus, Grafana, Loki, Jaeger.
DISTRO      = $(shell . /etc/os-release && echo $$ID)
# Files shipped in packages. Never add .env or config/alertmanager.yml: both hold secrets.
PKG_FILES   = docker-compose.yml Makefile README.md alerts config provisioning .env.example

.PHONY: all up down logs restart status ps config stage deb rpm install clean

all: install

.env:
	cp .env.example .env
	@echo ".env created from .env.example; set your Telegram credentials in it and re-run."
	@exit 1

config/alertmanager.yml: config/alertmanager.yml.tmpl .env
	@command -v envsubst >/dev/null 2>&1 || { echo "envsubst not found (install gettext)."; exit 1; }
	set -a; . ./.env; set +a; envsubst '$${TELEGRAM_API_KEY} $${TELEGRAM_CHAT_ID}' < $< > $@

config: config/alertmanager.yml

up: config
	docker compose up -d

down:
	docker compose down

logs:
	docker compose logs -f

restart: config
	docker compose restart

status ps:
	docker compose ps

stage:
	rm -rf $(PACKAGE_DIR)
	mkdir -p $(PACKAGE_DIR)/opt/$(NAME)
	cp -r $(PKG_FILES) $(PACKAGE_DIR)/opt/$(NAME)
	rm -f $(PACKAGE_DIR)/opt/$(NAME)/config/alertmanager.yml

deb: stage
	mkdir -p $(PACKAGE_DIR)/DEBIAN
	printf 'Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: %s\nDepends: docker.io, docker-compose, make, gettext-base\nDescription: %s\n' \
		"$(NAME)" "$(VERSION)" "$(ARCH)" "$(MAINTAINER)" "$(DESCRIPTION)" > $(PACKAGE_DIR)/DEBIAN/control
	dpkg-deb --build $(PACKAGE_DIR)
	mv $(PACKAGE_DIR).deb $(NAME)_$(VERSION)_$(ARCH).deb

rpm: stage
	@if ! command -v fpm >/dev/null 2>&1; then \
		echo "fpm not installed. Use: gem install fpm"; \
		exit 1; \
	fi
	fpm -s dir -t rpm -a noarch -n $(NAME) -v $(VERSION) --prefix /opt/$(NAME) \
		--maintainer "$(MAINTAINER)" \
		--depends docker --depends docker-compose --depends make --depends gettext \
		--description "$(DESCRIPTION)" \
		-C $(PACKAGE_DIR)/opt/$(NAME) .

install:
	@echo "Detecting distro: $(DISTRO)..."
ifeq ($(DISTRO),debian)
	$(MAKE) deb
	sudo dpkg -i $(NAME)_$(VERSION)_$(ARCH).deb
endif
ifeq ($(DISTRO),ubuntu)
	$(MAKE) deb
	sudo dpkg -i $(NAME)_$(VERSION)_$(ARCH).deb
endif
ifeq ($(DISTRO),fedora)
	$(MAKE) rpm
	sudo rpm -i $(NAME)-$(VERSION)-1.noarch.rpm
endif
	@echo "Installed $(NAME)!"

clean:
	rm -rf $(PACKAGE_DIR) *.deb *.rpm
