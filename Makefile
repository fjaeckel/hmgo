# Copyright (c) 2026 Frederic Jung
# SPDX-License-Identifier: BSD-3-Clause

VERSION ?= $(shell git describe --tags --match 'v*' --always --dirty 2>/dev/null || echo devel)
LDFLAGS := -s -w
GOFLAGS := -trimpath

.PHONY: build test dist clean gokrazy-vmtest

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
	GOOS=linux GOARCH=arm64       CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/hmgenconfig-$(VERSION)-linux-arm64 ./cmd/hmgenconfig
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/hmgenconfig-$(VERSION)-linux-armv7 ./cmd/hmgenconfig
	GOOS=linux GOARCH=arm64       CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/hmkeycheck-$(VERSION)-linux-arm64 ./cmd/hmkeycheck
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/hmkeycheck-$(VERSION)-linux-armv7 ./cmd/hmkeycheck
	cp LICENSE dist/LICENSE
	cd dist && sha256sum hmbridge-$(VERSION)-* hmsniff-$(VERSION)-* hmgenconfig-$(VERSION)-* hmkeycheck-$(VERSION)-* > SHA256SUMS-$(VERSION)

clean:
	rm -rf dist hmbridge

# Acceptance test of the gokrazy image (contrib/gokrazy): builds it,
# boots it in QEMU with an emulated radio module and updates it over
# the network. Runs in a container (Docker, e.g. OrbStack) with QEMU,
# mosquitto and gok; the Go module cache is kept in a volume.
gokrazy-vmtest:
	docker build -t hmbridge-vmtest contrib/gokrazy/vmtest
	docker run --rm -v $(CURDIR):/src -v hmbridge-vmtest-gomod:/root/go/pkg/mod -w /src \
		-e GOFLAGS=-buildvcs=false hmbridge-vmtest \
		go test -tags vmtest -v -count=1 -timeout 30m ./contrib/gokrazy/vmtest/
