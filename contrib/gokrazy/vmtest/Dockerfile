# Copyright (c) 2026 Frederic Jung
# SPDX-License-Identifier: BSD-3-Clause
#
# The environment of the gokrazy acceptance test (make gokrazy-vmtest):
# QEMU with arm64 UEFI firmware, mosquitto, mtools, Go and gok.
FROM alpine:3.23
# tzdata: gok copies the host's /etc/localtime into the image.
RUN apk add --no-cache qemu-system-aarch64 aavmf mosquitto mtools ca-certificates curl git tzdata && \
	ln -sf /usr/share/zoneinfo/UTC /etc/localtime
ARG GO_VERSION=1.27.4
RUN arch=$(apk --print-arch | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/') && \
	curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" | tar -C /usr/local -xz
ENV PATH=/usr/local/go/bin:/root/go/bin:$PATH \
	QEMU_EFI=/usr/share/AAVMF/QEMU_EFI.fd
# gok, pinned (the gokrazy tools have no releases).
ARG GOK_VERSION=v0.0.0-20261116075554-27173734feb0
RUN go install github.com/gokrazy/tools/cmd/gok@${GOK_VERSION} && rm -rf /root/.cache/go-build
