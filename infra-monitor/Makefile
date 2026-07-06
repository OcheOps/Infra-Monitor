NAME        = infra-monitor
VERSION     = 1.0.0
ARCH        = all
PACKAGE_DIR = $(NAME)-pkg
MAINTAINER  = Oche <gideon.david2015@gmail.com>
DESCRIPTION = Self-hosted infra monitor with Prometheus, Grafana, Loki, Jaeger.
DISTRO      = $(shell . /etc/os-release && echo $$ID)

.PHONY: all up down logs restart status ps deb rpm install clean

all: install

up:
	docker compose up -d

down:
	docker compose down

logs:
	docker compose logs -f

restart:
	docker compose restart

status ps:
	docker compose ps

deb:
	rm -rf $(PACKAGE_DIR)
	mkdir -p $(PACKAGE_DIR)/DEBIAN
	mkdir -p $(PACKAGE_DIR)/opt/$(NAME)
	cp -r docker-compose.yml Makefile README.md alerts config provisioning .env.example $(PACKAGE_DIR)/opt/$(NAME)
	printf 'Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: %s\nDepends: docker.io, docker-compose, curl\nDescription: %s\n' \
		"$(NAME)" "$(VERSION)" "$(ARCH)" "$(MAINTAINER)" "$(DESCRIPTION)" > $(PACKAGE_DIR)/DEBIAN/control
	dpkg-deb --build $(PACKAGE_DIR)
	mv $(PACKAGE_DIR).deb $(NAME)_$(VERSION)_$(ARCH).deb

rpm:
	@if ! command -v fpm >/dev/null 2>&1; then \
		echo "fpm not installed. Use: gem install fpm"; \
		exit 1; \
	fi
	fpm -s dir -t rpm -n $(NAME) -v $(VERSION) --prefix /opt/$(NAME) \
		--maintainer "$(MAINTAINER)" \
		--depends docker --depends docker-compose --depends curl \
		--description "$(DESCRIPTION)" \
		docker-compose.yml Makefile README.md alerts config provisioning .env.example

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
