# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

.PHONY: build bpf test fmt vet selftest selftest-multivip selftest-weighted selftest-ratelimit selftest-ipv6 selftest-ndp selftest-restart selftest-adopt selftest-drops selftest-xdpmode selftest-httpcheck selftest-apiauth selftest-affinity selftest-vipratelimit selftest-portrange selftest-edgecases selftest-l3dsr selftest-bgp selftest-checksum selftest-all \
	web web-install docs-serve docs-build \
	deploy deploy-remote deploy-remote-quick deploy-remote-preflight deploy-remote-verify deploy-remote-uninstall deploy-remote-fleet \
	sync-chart check-chart-sync build-cli release-cli install

CLANG ?= clang
ARCH  := $(shell uname -m)

all: bpf build

web-install:
	cd web && npm install

web: ## Build Netra-matching console into internal/api/ui (embedded by rivorad)
	cd web && npm run build

build: sync-chart
	@if [ ! -f internal/api/ui/index.html ]; then \
		echo "internal/api/ui missing — building web console…"; \
		$(MAKE) web; \
	fi
	go build -o bin/rivorad ./cmd/rivorad
	go build -o bin/rivoractl ./cmd/rivoractl
	go build -o bin/rivora-doctor ./cmd/rivora-doctor
	go build -o bin/rivora-controller ./cmd/rivora-controller
	go build -o bin/rivora ./cmd/rivora

build-cli: sync-chart ## Build just the rivora cluster-install CLI
	go build -o bin/rivora ./cmd/rivora

# Usage: make install [INSTALL_DIR=~/bin]. Same install-vs-sudo logic as
# scripts/install-cli.sh, for the local-build path instead of a downloaded
# release tarball.
INSTALL_DIR ?= /usr/local/bin
install: build-cli
	@if [ -w "$(INSTALL_DIR)" ]; then \
		install -m755 bin/rivora "$(INSTALL_DIR)/rivora"; \
	else \
		echo "sudo required to write to $(INSTALL_DIR)"; \
		sudo install -m755 bin/rivora "$(INSTALL_DIR)/rivora"; \
	fi
	@echo "installed $(INSTALL_DIR)/rivora ($$($(INSTALL_DIR)/rivora version))"

# Cross-compiles the rivora CLI for every platform the release workflow
# publishes (.github/workflows/release.yml's build-cli job) — lets you
# produce real release artifacts locally without pushing a tag.
# Usage: make release-cli VERSION=v0.3.0
RELEASE_PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
release-cli: sync-chart
	@test -n "$(VERSION)" || { echo "  VERSION is required, e.g. make release-cli VERSION=v0.3.0"; exit 1; }
	@mkdir -p dist
	@for platform in $(RELEASE_PLATFORMS); do \
		goos=$${platform%/*}; goarch=$${platform#*/}; \
		name="rivora_$(VERSION)_$${goos}_$${goarch}"; \
		echo "building $$name..."; \
		mkdir -p "dist/$$name"; \
		GOOS=$$goos GOARCH=$$goarch CGO_ENABLED=0 go build -ldflags "-X main.version=$(VERSION)" -o "dist/$$name/rivora" ./cmd/rivora || exit 1; \
		tar czf "dist/$$name.tar.gz" -C dist "$$name/rivora"; \
		( cd dist && (sha256sum "$$name.tar.gz" 2>/dev/null || shasum -a 256 "$$name.tar.gz") > "$$name.tar.gz.sha256" ); \
		rm -rf "dist/$$name"; \
	done
	@echo "release artifacts in dist/"

# internal/installer embeds a copy of deploy/helm/rivora (go:embed can't
# reach outside its own package directory) — deploy/helm/rivora stays the
# one human-facing source of truth; this copy is a generated build
# artifact. check-chart-sync (run in CI) fails if they've drifted.
sync-chart:
	rm -rf internal/installer/chartdata/rivora
	mkdir -p internal/installer/chartdata
	cp -r deploy/helm/rivora internal/installer/chartdata/rivora

check-chart-sync: sync-chart
	git diff --exit-code internal/installer/chartdata || \
		{ echo "internal/installer/chartdata is out of sync with deploy/helm/rivora — run 'make sync-chart' and commit the result"; exit 1; }

# Linux-only: needs linux/bpf.h and friends. Run on the remote build host,
# not on macOS — see scripts/deploy-remote.sh.
bpf:
	$(CLANG) -target bpfel -O2 -g -Wall -Wextra -Werror \
		-I/usr/include/$(ARCH)-linux-gnu -c bpf/xdp_ingress.c -o bpf/xdp_ingress.o
	$(CLANG) -target bpfel -O2 -g -Wall -Wextra -Werror \
		-I/usr/include/$(ARCH)-linux-gnu -c bpf/tc_nat.c -o bpf/tc_nat.o

test:
	go test ./...

fmt:
	gofmt -w cmd internal

vet:
	go vet ./...

selftest:
	bash scripts/selftest.sh

selftest-multivip:
	bash scripts/selftest-multivip.sh

selftest-weighted:
	bash scripts/selftest-weighted.sh

selftest-ratelimit:
	bash scripts/selftest-ratelimit.sh

selftest-affinity:
	bash scripts/selftest-affinity.sh

selftest-vipratelimit:
	bash scripts/selftest-vipratelimit.sh

selftest-portrange:
	bash scripts/selftest-portrange.sh

selftest-edgecases:
	bash scripts/selftest-edgecases.sh

selftest-l3dsr:
	bash scripts/selftest-l3dsr.sh

selftest-bgp:
	bash scripts/selftest-bgp.sh

selftest-ipv6:
	bash scripts/selftest-ipv6.sh

selftest-ndp:
	bash scripts/selftest-ndp.sh

selftest-restart:
	bash scripts/selftest-restart.sh

selftest-adopt:
	bash scripts/selftest-adopt.sh

selftest-drops:
	bash scripts/selftest-drops.sh

selftest-xdpmode:
	bash scripts/selftest-xdpmode.sh

selftest-httpcheck:
	bash scripts/selftest-httpcheck.sh

selftest-apiauth:
	bash scripts/selftest-apiauth.sh

selftest-all: selftest selftest-multivip selftest-weighted selftest-ratelimit selftest-ipv6 selftest-ndp selftest-restart selftest-adopt selftest-drops selftest-xdpmode selftest-httpcheck selftest-apiauth selftest-affinity selftest-vipratelimit selftest-portrange selftest-edgecases selftest-l3dsr selftest-bgp selftest-checksum
selftest-checksum:
	bash scripts/selftest-checksum.sh


docs-serve: ## Local Docusaurus preview (website/)
	npm --prefix website start

docs-build: ## Production Docusaurus build into website/build/
	npm --prefix website ci
	npm --prefix website run build

deploy: deploy-remote ## Alias for deploy-remote

deploy-remote: ## Full remote deploy: make deploy-remote H=ip U=user
	@test -n "$(H)" || { echo "  H (host) is required"; exit 1; }
	bash scripts/deploy-remote.sh $(H) $(or $(U),sus) --key $(ARGS)

deploy-remote-quick: ## Quick remote deploy: make deploy-remote-quick H=ip U=user
	@test -n "$(H)" || { echo "  H is required"; exit 1; }
	bash scripts/deploy-remote.sh $(H) $(or $(U),sus) --key --quick $(ARGS)

deploy-remote-preflight: ## SSH preflight only: make deploy-remote-preflight H=ip
	@test -n "$(H)" || { echo "  H is required"; exit 1; }
	bash scripts/deploy-remote.sh $(H) $(or $(U),sus) --key --preflight-only

deploy-remote-verify: ## Remote selftest only: make deploy-remote-verify H=ip
	@test -n "$(H)" || { echo "  H is required"; exit 1; }
	bash scripts/deploy-remote.sh $(H) $(or $(U),sus) --key --verify-only

deploy-remote-uninstall: ## Remove rivora from host: make deploy-remote-uninstall H=ip
	@test -n "$(H)" || { echo "  H is required"; exit 1; }
	bash scripts/deploy-remote.sh $(H) $(or $(U),sus) --key --uninstall

deploy-remote-fleet: ## Fleet deploy: make deploy-remote-fleet FILE=hosts.txt
	@test -n "$(FILE)" || { echo "  FILE is required"; exit 1; }
	bash scripts/deploy-remote.sh --fleet $(FILE)
