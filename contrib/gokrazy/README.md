<!-- SPDX-License-Identifier: BSD-3-Clause -->

# hmbridge on gokrazy

[gokrazy](https://gokrazy.org/) turns a Raspberry Pi into an appliance that
runs only Go programs: no shell, no package manager, a read-only root file
system. Updates go over the network to the inactive one of two root
partitions, which then boots, so a failed transfer never leaves a
half-written system. This directory builds an SD card image that runs
hmbridge and nothing else.

| partition | content | on update |
|---|---|---|
| boot (FAT) | Raspberry Pi firmware, kernel, `config.txt` | replaced |
| root A / root B (SquashFS, read-only) | gokrazy, hmbridge, `/etc/hmbridge/config.json` | the inactive one is written, then booted |
| `/perm` (ext4) | `/perm/hmbridge`: `system.key`, `devices.json`, `state.json`, `settings.json` | kept |

**Supported:** whatever gokrazy supports (64-bit): Raspberry Pi 3, 4, 5,
Zero 2 W (see gokrazy's [platform list](https://gokrazy.org/platforms/)). Both radio modules
(HM-MOD-RPI-PCB, RPI-RF-MOD) sit on the header's UART (`/dev/ttyAMA0`) and
reset line, as on Raspberry Pi OS.

## What the image contains

* gokrazy's Raspberry Pi kernel (Raspberry Pi Foundation kernel). Every
  driver hmbridge needs is built in: the PL011 UART, the GPIO character
  device (reset line), and, for the RPI-RF-MOD, I²C, `rtc-ds1307` (its
  RX8130 clock) and `leds-gpio`. No eQ-3 kernel modules are needed:
  hmbridge talks to the module directly.
* `config.txt`: `enable_uart=1` and the PL011 UART on GPIO 14/15
  (`dtoverlay=disable-bt` on the Pi 3, 4 and Zero 2: no Bluetooth;
  `dtparam=uart0=on` on the Pi 5). No kernel console on it
  (`"SerialConsole": "disabled"`).
* The gokrazy system programs (init, DHCP, NTP, web interface for updates)
  and [`gokrazy/mkfs`](https://github.com/gokrazy/mkfs), which creates the
  `/perm` file system at the first boot (and then reboots once).
* hmbridge, started with `-create_key_file`: at its first start it creates
  `/perm/hmbridge/system.key` with a new random AES system key. It refuses
  to do so when devices are already paired (`devices.json` exists) or
  configured, because those use the lost key.

## Building and installing

Needs Go and [gok](https://gokrazy.org/quickstart/)
(`go install github.com/gokrazy/tools/cmd/gok@main`). Runs on Linux and
macOS.

```sh
# 1. Create the instance (asks for the MQTT broker, user and password,
#    or takes MQTT_BROKER, MQTT_USERNAME, MQTT_PASSWORD). The directory
#    name is the host name.
contrib/gokrazy/new-instance.sh ~/gokrazy/hmbridge

# 2. Write it to an SD card (the whole card; check the device name!) ...
gok --parent_dir ~/gokrazy -i hmbridge overwrite --full /dev/sdX
#    ... or to an image file for Raspberry Pi Imager / dd:
gok --parent_dir ~/gokrazy -i hmbridge overwrite --full hmbridge.img --target_storage_bytes=2000000000
```

Plug the card into the Pi (Ethernet, or see gokrazy's
[Wi-Fi](https://gokrazy.org/packages/wifi/) package) and power it up. The
first boot formats `/perm` and reboots. Then hmbridge appears in Home
Assistant. Pair your devices as described in
[docs/DEPLOY.md](../../docs/DEPLOY.md#step-10-pair-the-devices-one-by-one).

The instance directory holds:

* `config.json`: gokrazy's configuration, including the password of the
  web interface and update API (`Update.HTTPPassword`). The web interface
  at `http://gokrazy:<password>@hmbridge/` shows the processes and
  hmbridge's log.
* `hmbridge.json`: hmbridge's configuration. It is built into the image as
  `/etc/hmbridge/config.json` (see
  [internal/config/example.json](../../internal/config/example.json)). It
  holds a random `central_address`, the MQTT settings,
  `"serial_port": "/dev/ttyAMA0"` (gokrazy has no udev, so there is no
  `/dev/serial0`) and `"state_dir": "/perm/hmbridge"`.
* `builddir/`: the Go module hmbridge is built in. By default it points at
  this checkout (a `replace` directive). With `new-instance.sh -version
  vX.Y.Z` it uses a released version instead, and `gok get` updates it.

Keep the instance directory. It is the source of every update, and its
`central_address` must stay the same.

## Updating

```sh
git pull                      # or: gok --parent_dir ~/gokrazy -i hmbridge get   (with -version)
gok --parent_dir ~/gokrazy -i hmbridge update
```

`gok update` builds the image, sends the new root file system and kernel
to the Pi over HTTP, switches the active root partition and reboots.
`/perm` stays as it is. A change to `hmbridge.json` is also deployed with
`gok update`.

## Backing up the key

Without `/perm/hmbridge/system.key`, every AES device must be
factory-reset and paired again. gokrazy has no shell, so to fetch the key,
add the [breakglass](https://github.com/gokrazy/breakglass) package
temporarily (`gok add github.com/gokrazy/breakglass`, then `gok update`),
copy `/perm/hmbridge/system.key` and `devices.json` with `scp`, and remove
it again. To move an existing installation to gokrazy, put its key file
(and `devices.json`) on the new card's `/perm/hmbridge/` the same way
*before* hmbridge first starts. Alternatively, put the key in
`hmbridge.json` (`"security": {"key": "…"}`), but then it is part of the
image.

## Clock

A Raspberry Pi has no real-time clock. gokrazy starts hmbridge only once
the clock has been set (`WaitForClock`). That happens from the time saved
at the last shutdown, then from NTP (servers from DHCP, or the gokrazy
pool). gokrazy's NTP client sets the clock without marking the kernel
clock as synchronised. hmbridge's `require_synced_clock` check would
therefore never pass, so it is switched off in `hmbridge.json`. As a
result, right after a long power cut the devices can get the saved,
outdated time once; the next hourly time sync (`time_sync_interval`)
corrects it.

## Acceptance test

[vmtest/](vmtest/) builds the image for QEMU (`new-instance.sh -vm`:
gokrazy's generic arm64 kernel, the radio module on a virtio console) and
boots it with an emulated HM-MOD-RPI-PCB and a mosquitto broker. The test
plays Home Assistant: it waits for the first boot (mkfs, reboot, key
creation), checks MQTT discovery and the HTTP API, and pairs a window
contact, which gets the key generated on the device. It then runs
`gok update` against the running VM, waits for the new version and checks
that the key and the paired device survived the update. A second test
builds the Raspberry Pi image and checks its `config.txt` and
`cmdline.txt`.

```sh
make gokrazy-vmtest           # in a container (Docker or OrbStack), see vmtest/Dockerfile
```

Not tested in QEMU: the Raspberry Pi kernel and firmware, the real UART
and GPIO reset line. For those, [hwtest/](hwtest/README.md) tests a real
Pi from another computer (bring-up, pairing each device model, reboots,
updates, power cuts), with a runbook for the Raspberry Pi 3 B+ with an
RPI-RF-MOD. `new-instance.sh -debug` makes an image for that: SSH
(breakglass), every radio frame in the log, I²C on.
