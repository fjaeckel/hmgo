<!-- SPDX-License-Identifier: BSD-3-Clause -->

# QEMU acceptance test of the gokrazy image

`vmtest_test.go` tests the gokrazy image ([../README.md](../README.md))
end to end, without hardware. CI runs it on every push (job `gokrazy`).

```sh
make gokrazy-vmtest
```

This builds the container in [Dockerfile](Dockerfile) and runs the tests
in it. The container (Alpine) has QEMU with arm64 UEFI firmware,
mosquitto, mtools, Go and a pinned gok, and the Go module cache stays in
the Docker volume `hmbridge-vmtest-gomod`. It works with Docker or
OrbStack, on arm64 and amd64 hosts. The whole run takes about two minutes
on an Apple Silicon Mac.

On a Linux host that has the tools itself:

```sh
go test -tags vmtest -v -count=1 -timeout 30m ./contrib/gokrazy/vmtest/
```

| variable | meaning |
|---|---|
| `GOK` | the gok binary (default: from `PATH`) |
| `QEMU_EFI` | arm64 UEFI firmware (default `/usr/share/qemu-efi-aarch64/QEMU_EFI.fd`; the container sets Alpine's path) |
| `VMTEST_KEEP=1` | keep the work directory (`console.log` with the guest's kernel console, the image, the instance) and print its path |

A missing tool skips the test instead of failing it.

## TestGokrazyImage

The test builds an instance with `new-instance.sh -vm`. It uses gokrazy's
generic arm64 kernel (EFI) instead of the Raspberry Pi's, and puts the
radio module on a virtio console (`/dev/hvc0`) instead of the Pi's UART.
It boots that instance in QEMU (`virt` machine, KVM if available).

On the host side it runs:

* an **emulated HM-MOD-RPI-PCB** (`internal/uartgw/fakemod`), served to
  QEMU's character device over TCP. QEMU keeps that connection across guest
  reboots, so the module persists like a real one;
* **mosquitto**, which the guest reaches as `10.0.2.2`;
* a **Home Assistant stand-in** on MQTT.

Port forwards give the host the gokrazy API (for `gok update`) and
hmbridge's HTTP API.

The test checks, in order:

1. **First boot:** `/perm` is formatted by mkfs and the guest reboots
   once. hmbridge creates the AES system key on `/perm` and sets up the
   module with it: a new key, index 1, the instance's central address.
2. **Running system:** availability `online`, MQTT discovery,
   `/api/v1/health`, `/metrics` (radio up, MQTT connected, the version
   built in, no insecure permissions).
3. **Pairing:** a window contact pairs from "Home Assistant" and gets the
   key generated on the device. Its authenticated event arrives as state.
4. **The hardware test against the VM:** `hwtest`'s `TestBringUp`,
   `TestPairedDevices` and `TestReboot` (one reboot), so the harness used
   on real Pis is itself tested.
5. **Update:** `gok update` with a new version number. The guest switches
   root partitions and reboots. The new version runs, the module gets the
   same key again (a lost `/perm` would have made a new one), and the
   paired contact's next event arrives.

## TestRaspberryPiImage

The test builds the Raspberry Pi image as `new-instance.sh` makes it, and
reads its boot partition with mtools. It checks:

* `config.txt` has `enable_uart=1`, `dtoverlay=disable-bt` for the Pi 3, 4
  and Zero 2, `dtparam=uart0=on` for the Pi 5, and the LED settings;
* `cmdline.txt` has no console on `serial0` or `ttyAMA0`;
* the device trees for the Pi 4 and 5, the `disable-bt` overlay and the
  kernel are present.

## What it does not cover

* **The Raspberry Pi kernel and firmware.** QEMU's `virt` machine boots
  gokrazy's generic kernel. QEMU's `raspi3b` emulation would run the Pi
  kernel but not the firmware or `config.txt`.
* **The real UART, the GPIO reset line, and the module's reset
  behaviour.** The guest runs with `"reset": "none"`. The emulated module
  does not model reset polarities; the RPI-RF-MOD's (active high) was
  found on hardware.
* **The RPI-RF-MOD.** The VM uses the emulated HM-MOD-RPI-PCB.
  `internal/uartgw` and `cmd/hmbridge` test the RPI-RF-MOD protocol and
  detection against the emulated module in unit tests.

For these, see [../hwtest/](../hwtest/README.md).
