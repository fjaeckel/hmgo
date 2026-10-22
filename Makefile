# Copyright (c) 2026 Frederic Jung
# SPDX-License-Identifier: BSD-3-Clause

VERSION ?= $(shell git describe --tags --match 'v*' --always --dirty 2>/dev/null || echo devel)
LDFLAGS := -s -w
GOFLAGS := -trimpath

.PHONY: build test dist clean

build:
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o hmbridge ./cmd/hmbridge

test:
	go vet ./...
	go test -race ./...

dist:
	mkdir -p dist
	GOOS=linux GOARCH=arm64       CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/hmbridge-$(VERSION)-linux-arm64 ./cmd/hmbridge
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/hmbridge-$(VERSION)-linux-armv7 ./cmd/hmbridge
	GOOS=linux GOARCH=arm GOARM=6 CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/hmbridge-$(VERSION)-linux-armv6 ./cmd/hmbridge
	GOOS=linux GOARCH=amd64       CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/hmbridge-$(VERSION)-linux-amd64 ./cmd/hmbridge
	GOOS=linux GOARCH=arm64       CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/hmsniff-$(VERSION)-linux-arm64 ./cmd/hmsniff
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/hmsniff-$(VERSION)-linux-armv7 ./cmd/hmsniff
	cp LICENSE dist/LICENSE
	cd dist && sha256sum hmbridge-$(VERSION)-* hmsniff-$(VERSION)-* > SHA256SUMS-$(VERSION)

clean:
	rm -rf dist hmbridge
