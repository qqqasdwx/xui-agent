SHELL := bash

.PHONY: test
test:
	go test ./...

.PHONY: verify
verify:
	go test -race ./...
	go vet ./...
	sh -n deploy/*.sh deploy/*launcher deploy/*.openrc scripts/*.sh

.PHONY: integration-systemd
integration-systemd:
	./scripts/test-systemd-integration.sh

.PHONY: integration-install-upgrade
integration-install-upgrade:
	./scripts/test-install-upgrade.sh

.PHONY: build
build:
	CGO_ENABLED=0 go build -trimpath -o bin/xui-agent ./cmd/xui-agent

.PHONY: release-snapshot
release-snapshot:
	./scripts/build-release.sh dev dist
