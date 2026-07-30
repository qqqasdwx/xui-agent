SHELL := bash

.PHONY: test
test:
	go test ./...

.PHONY: verify
verify:
	go test -race ./...
	go vet ./...

.PHONY: build
build:
	CGO_ENABLED=0 go build -trimpath -o bin/xui-agent ./cmd/xui-agent

.PHONY: release-snapshot
release-snapshot:
	./scripts/build-release.sh dev dist
