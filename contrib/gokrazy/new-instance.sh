#!/bin/sh
# Copyright (c) 2026 Frederic Jung
# SPDX-License-Identifier: BSD-3-Clause
#
# new-instance.sh creates a gokrazy instance that runs hmbridge (see
# README.md in this directory):
#
#	contrib/gokrazy/new-instance.sh [-vm] [-debug] [-version vX.Y.Z] <instance directory>
#
# The directory's name is the host name. It gets:
#   config.json   gokrazy's configuration (packages, boot settings,
#                 update password);
#   hmbridge.json hmbridge's configuration, built into the image as
#                 /etc/hmbridge/config.json; the AES system key is
#                 created on the device at the first start
#                 (/perm/hmbridge/system.key, -create_key_file);
#   builddir/     the Go module gokrazy builds hmbridge in: this
#                 checkout (default) or a released version (-version).
#
# The MQTT settings are asked for, or taken from MQTT_BROKER,
# MQTT_USERNAME and MQTT_PASSWORD. -vm makes an instance for QEMU
# (gok vm run, the acceptance test in vmtest/): gokrazy's generic arm64
# kernel, the radio module on the virtio console /dev/hvc0, no reset
# line. -debug makes an instance for testing on hardware (see
# hwtest/README.md): hmbridge logs every radio frame (-debug), SSH
# access with a busybox shell (gokrazy's breakglass, for the public key
# in BREAKGLASS_KEYS or ~/.ssh/id_ed25519.pub, id_ecdsa.pub, id_rsa.pub)
# and the I2C bus of the header on (the RPI-RF-MOD's clock at 0x32).
# Existing files are never overwritten.
set -eu

vm=0
debug=0
version=
console=disabled
while [ $# -gt 1 ]; do
	case "$1" in
	-vm) vm=1 ;;
	-debug) debug=1 ;;
	-version) shift; version=$1 ;;
	*) break ;;
	esac
	shift
done
if [ $# -ne 1 ]; then
	echo "usage: $0 [-vm] [-debug] [-version vX.Y.Z] <instance directory>" >&2
	exit 2
fi
dir=$1
name=$(basename "$dir")
case "$name" in
*[!a-zA-Z0-9-]* | -* | "") echo "$0: the directory name $name is the host name: letters, digits and - only" >&2; exit 2 ;;
esac
for f in "$dir/config.json" "$dir/hmbridge.json"; do
	if [ -e "$f" ]; then
		echo "$0: $f already exists; not overwriting it" >&2
		exit 1
	fi
done
repo=$(cd "$(dirname "$0")/../.." && pwd)
pkg=github.com/fjaeckel/hmgo/cmd/hmbridge

# -debug: the SSH key for breakglass.
if [ $debug = 1 ]; then
	keys=${BREAKGLASS_KEYS:-}
	if [ -z "$keys" ]; then
		for k in "$HOME/.ssh/id_ed25519.pub" "$HOME/.ssh/id_ecdsa.pub" "$HOME/.ssh/id_rsa.pub"; do
			if [ -f "$k" ]; then
				keys=$k
				break
			fi
		done
	fi
	if [ -z "$keys" ] || [ ! -f "$keys" ]; then
		echo "$0: -debug needs an SSH public key for breakglass: set BREAKGLASS_KEYS to an authorized_keys file" >&2
		exit 1
	fi
fi

hex() { od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'; }

# The central address: hmbridge's radio address, random in
# 100000..EFFFFF like hmbridge init (see docs/DEPLOY.md).
central=$(printf '%06X' $(( 0x100000 + 0x$(hex 3) % 0xE00000 )))

if [ $vm = 1 ]; then
	: "${MQTT_BROKER:=tcp://10.0.2.2:1883}"
	: "${MQTT_USERNAME:=}"
	: "${MQTT_PASSWORD:=}"
	serial=/dev/hvc0
	reset=none
else
	if [ -z "${MQTT_BROKER+x}" ]; then
		printf 'MQTT broker of Home Assistant [tcp://homeassistant.local:1883]: '
		read -r MQTT_BROKER
		: "${MQTT_BROKER:=tcp://homeassistant.local:1883}"
	fi
	if [ -z "${MQTT_USERNAME+x}" ]; then
		printf 'MQTT user name [hmbridge]: '
		read -r MQTT_USERNAME
		: "${MQTT_USERNAME:=hmbridge}"
	fi
	if [ -z "${MQTT_PASSWORD+x}" ] && [ -n "$MQTT_USERNAME" ]; then
		printf 'MQTT password (not shown): '
		stty -echo 2>/dev/null || true
		read -r MQTT_PASSWORD
		stty echo 2>/dev/null || true
		echo
	fi
	: "${MQTT_PASSWORD:=}"
	serial=/dev/ttyAMA0
	reset=gpio
fi

# JSON string escaping for the values asked for.
js() { printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'; }

mkdir -p "$dir"
umask 077
cat >"$dir/hmbridge.json" <<JSON
{
  "_comment": [
    "hmbridge on gokrazy, made by contrib/gokrazy/new-instance.sh. See internal/config/example.json for all options.",
    "This file is built into the image as /etc/hmbridge/config.json: after changing it, run gok update.",
    "The AES system key is created on the device at the first start (/perm/hmbridge/system.key). Back it up: without it, every AES device must be factory-reset and paired again.",
    "require_synced_clock is false because gokrazy's NTP client does not mark the kernel clock as synchronised; gokrazy starts hmbridge only once the clock is set (WaitForClock in config.json)."
  ],
  "serial_port": "$serial",
  "radio": "auto",
  "reset": "$reset",
  "central_address": "$central",
  "security": { "key_file": "/perm/hmbridge/system.key", "index": 1 },
  "mqtt": { "broker": "$(js "$MQTT_BROKER")", "username": "$(js "$MQTT_USERNAME")", "password": "$(js "$MQTT_PASSWORD")" },
  "require_synced_clock": false,
  "state_dir": "/perm/hmbridge",
  "devices": []
}
JSON

if [ $vm = 1 ]; then
	# The kernel console goes to the first serial port (QEMU -serial),
	# the radio module is on the virtio console.
	boot='
  "KernelPackage": "github.com/gokrazy/kernel.arm64",
  "FirmwarePackage": "",
  "EEPROMPackage": "",'
	console=ttyAMA0,115200
else
	# The radio module needs the PL011 UART on GPIO 14/15 (/dev/ttyAMA0):
	# Bluetooth off on the Pi 3, 4 and Zero 2; UART0 on the Pi 5's
	# header. SerialConsole "disabled" keeps the kernel console off it.
	# LEDs: the red power LED (always on, it only says "powered") is
	# switched off, and the green activity LED blinks a heartbeat while
	# the system runs.
	i2c=
	if [ $debug = 1 ]; then
		i2c=', "dtparam=i2c_arm=on"'
	fi
	boot='
  "BootloaderExtraLines": [
    "[pi3]", "dtoverlay=disable-bt",
    "[pi4]", "dtoverlay=disable-bt",
    "[pi02]", "dtoverlay=disable-bt",
    "[pi5]", "dtparam=uart0=on",
    "[all]", "dtparam=pwr_led_trigger=none", "dtparam=act_led_trigger=heartbeat"'"$i2c"'
  ],'
fi

pkgs='"'"$pkg"'", "github.com/gokrazy/mkfs"'
hmflags='"-config=/etc/hmbridge/config.json", "-create_key_file"'
pkgcfg=
if [ $debug = 1 ]; then
	mkdir -p "$dir"
	cp "$keys" "$dir/breakglass.authorized_keys"
	pkgs="$pkgs"', "github.com/gokrazy/breakglass", "github.com/gokrazy/serial-busybox"'
	hmflags="$hmflags"', "-debug"'
	pkgcfg='
    "github.com/gokrazy/breakglass": {
      "CommandLineFlags": ["-authorized_keys=/etc/breakglass.authorized_keys"],
      "ExtraFilePaths": { "/etc/breakglass.authorized_keys": "breakglass.authorized_keys" }
    },'
fi

cat >"$dir/config.json" <<JSON
{
  "Hostname": "$name",
  "Update": { "HTTPPassword": "$(hex 12)" },
  "Environment": ["GOOS=linux", "GOARCH=arm64"],
  "Packages": [$pkgs],
  "PackageConfig": {$pkgcfg
    "$pkg": {
      "CommandLineFlags": [$hmflags],
      "ExtraFilePaths": { "/etc/hmbridge/config.json": "hmbridge.json" },
      "WaitForClock": true
    },
    "github.com/gokrazy/gokrazy/cmd/randomd": {
      "ExtraFileContents": { "/etc/machine-id": "$(hex 16)\n" }
    }
  },$boot
  "SerialConsole": "$console"
}
JSON

# The Go module gokrazy builds hmbridge in (what gok add does).
build="$dir/builddir/$pkg"
mkdir -p "$build"
(
	cd "$build"
	[ -e go.mod ] || go mod init "gokrazy/build/$pkg" >/dev/null 2>&1
	if [ -n "$version" ]; then
		go get "$pkg@$version"
	else
		go mod edit -replace "github.com/fjaeckel/hmgo=$repo" \
			-require "github.com/fjaeckel/hmgo@v0.0.0-00010101000000-000000000000"
		cat >tools.go <<GO
//go:build tools

package tools

import _ "$pkg"
GO
		go mod tidy >/dev/null
	fi
)

echo "created gokrazy instance $name in $dir (central address $central, radio on $serial)"
echo "next: gok --parent_dir $(cd "$dir/.." && pwd) -i $name overwrite --full /dev/sdX   (see contrib/gokrazy/README.md)"
