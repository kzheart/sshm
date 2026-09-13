GO ?= go
VERSION ?= 0.1.0
PREFIX ?= $(HOME)/.local

.PHONY: build test check integration install dist

build:
	$(GO) build -trimpath -ldflags '-s -w -X main.version=$(VERSION)' -o bin/sshm .

test:
	$(GO) test -race ./...

check: test
	$(GO) vet ./...
	test -z "$$($(GO) fmt ./...)"

integration:
	docker build -t sshm-test-server:local -f tests/Dockerfile tests
	python3 tests/integration.py

install: build
	./install.sh --prefix "$(PREFIX)"

dist:
	./scripts/release.sh "$(VERSION)"
