# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

.PHONY: build bpf test fmt vet selftest \
	deploy deploy-remote deploy-remote-quick deploy-remote-preflight deploy-remote-verify deploy-remote-uninstall deploy-remote-fleet

CLANG ?= clang
ARCH  := $(shell uname -m)

all: bpf build

build:
	go build -o bin/rivorad ./cmd/rivorad
	go build -o bin/rivoractl ./cmd/rivoractl
	go build -o bin/rivora-doctor ./cmd/rivora-doctor

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
