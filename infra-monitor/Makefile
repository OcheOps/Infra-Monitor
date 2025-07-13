NAME = infra-monitor
VERSION = 1.0.0
ARCH = all
PACKAGE_DIR = $(NAME)-pkg
DISTRO = $(shell . /etc/os-release && echo $ID)

.PHONY: all deb rpm install clean

all: install

deb:
	rm -rf $(PACKAGE_DIR)
	mkdir -p $(PACKAGE_DIR)/DEBIAN
	mkdir -p $(PACKAGE_DIR)/opt/$(NAME)
	cp -r * $(PACKAGE_DIR)/opt/$(NAME)
	echo "Package: $(NAME)\nVersion: $(VERSION)\nArchitecture: $(ARCH)\nMaintainer: Your Name <you@example.com>\nDepends: docker.io, docker-compose, curl\nDescription: Self-hosted infra monitor with Prometheus, Grafana, Loki, Jaeger." > $(PACKAGE_DIR)/DEBIAN/control
	dpkg-deb --build $(PACKAGE_DIR)
	mv $(PACKAGE_DIR).deb $(NAME)_$(VERSION)_$(ARCH).deb

rpm:
	@if ! command -v fpm >/dev/null 2>&1; then 		echo "❌ fpm not installed. Use: gem install fpm"; 		exit 1; 	fi
	fpm -s dir -t rpm -n $(NAME) -v $(VERSION) --prefix /opt/$(NAME) 		--depends docker --depends docker-compose --depends curl 		--description "Self-hosted infra monitor with Prometheus, Grafana, Loki, Jaeger." 		setup.sh docker-compose.yml config provisioning alerts .env

install:
	@echo "📦 Detecting distro: $(DISTRO)..."
ifeq ($(DISTRO),debian)
	make deb
	sudo dpkg -i $(NAME)_$(VERSION)_$(ARCH).deb
endif
ifeq ($(DISTRO),ubuntu)
	make deb
	sudo dpkg -i $(NAME)_$(VERSION)_$(ARCH).deb
endif
ifeq ($(DISTRO),fedora)
	make rpm
	sudo rpm -i $(NAME)-$(VERSION)-1.noarch.rpm
endif
	@echo "✅ Installed $(NAME)!"

clean:
	rm -rf $(PACKAGE_DIR) *.deb *.rpm
