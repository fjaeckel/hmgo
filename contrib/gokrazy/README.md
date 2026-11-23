<!-- SPDX-License-Identifier: BSD-3-Clause -->

# hmbridge on gokrazy

This directory builds an SD card image for a Raspberry Pi that runs
hmbridge and nothing else, on [gokrazy](https://gokrazy.org/):

* **Read-only root file system.** gokrazy has no shell, package manager
  or SSH server in the image, only Go programs.
* **Updates over the network.** `gok update` writes the new system to the
  inactive one of two root partitions, then reboots into it. A failed
  transfer never leaves a half-written system, and your configuration,
  key and paired devices on `/perm` are not touched.
* **One file to configure.** hmbridge's configuration lives next to the
  image definition on your computer and is built into the image.

It runs on gokrazy's 64-bit [platforms](https://gokrazy.org/platforms/):
Raspberry Pi 3, 4, 5 and Zero 2 W, with an HM-MOD-RPI-PCB or an
RPI-RF-MOD. It has been used on a **Raspberry Pi 3 B+ with an RPI-RF-MOD**
(firmware 4.4.22). See [Tested on hardware](#tested-on-hardware) for what
has been verified.

Contents:

* [What you need](#what-you-need)
* [New installation](#new-installation)
* [Moving from OpenCCU](#moving-from-openccu)
* [Writing the SD card](#writing-the-sd-card)
* [First boot](#first-boot)
* [Operating it](#operating-it): logs, updates, configuration changes, going back
* [The debug image](#the-debug-image): SSH, every radio frame
* [Backing up the key](#backing-up-the-key)
* [How it works](#how-it-works)
* [Troubleshooting](#troubleshooting)
* [Tests](#tests)

## What you need

| | |
|---|---|
| Raspberry Pi | 3, 3 B+, 4, 5 or Zero 2 W, with Ethernet (the image has no Wi-Fi configuration; see gokrazy's [wifi](https://gokrazy.org/packages/wifi/) package) |
| Radio module | RPI-RF-MOD or HM-MOD-RPI-PCB on the header's first pins (pin 1 at the SD card end) |
| SD card | 4 GB or more; the image uses about 110 MB. Buy a high-endurance or industrial card (8–32 GB): SD cards in a Pi die of power cuts and wear, not of being full. |
| MQTT broker | e.g. the Mosquitto add-on of Home Assistant |
| On your computer (Linux or macOS) | Go and [gok](https://gokrazy.org/quickstart/): `go install github.com/gokrazy/tools/cmd/gok@main`, with `~/go/bin` on your `PATH` |

gok builds everything on your computer: it cross-compiles for the Pi and
writes the image itself, without Docker or Linux tools.

## New installation

```sh
contrib/gokrazy/new-instance.sh ~/gokrazy/hmbridge
```

This asks for the MQTT broker, user and password (or takes `MQTT_BROKER`,
`MQTT_USERNAME`, `MQTT_PASSWORD`). It then creates the **instance
directory** `~/gokrazy/hmbridge`, whose name becomes the Pi's host name:

| file | what it is |
|---|---|
| `config.json` | gokrazy's configuration: the packages, boot settings, and the password of the web interface and update API (`Update.HTTPPassword`) |
| `hmbridge.json` | hmbridge's configuration (see [internal/config/example.json](../../internal/config/example.json)). It is built into the image as `/etc/hmbridge/config.json`. It holds a random `central_address`, your MQTT settings, `"serial_port": "/dev/ttyAMA0"`, `"radio": "auto"` and `"state_dir": "/perm/hmbridge"`. |
| `builddir/` | the Go module gokrazy builds hmbridge in. By default it points at this checkout. With `new-instance.sh -version vX.Y.Z`, it uses a released version instead. |

The AES system key is **not** created here. hmbridge creates it on the Pi
at its first start (`/perm/hmbridge/system.key`, `-create_key_file`).

**Keep the instance directory.** Every update is built from it, and its
`central_address` must never change: your devices know it. It contains
your MQTT password (and, if you put it there, your key), so its files are
private (mode 0600).

All `gok` commands below use the default parent directory `~/gokrazy`. For
an instance elsewhere, add `--parent_dir <directory>`.

Continue with [Writing the SD card](#writing-the-sd-card). After the first
boot, pair your devices from Home Assistant as described in
[docs/DEPLOY.md](../../docs/DEPLOY.md#step-10-pair-the-devices-one-by-one).

## Moving from OpenCCU

If hmbridge already runs on OpenCCU (or on Raspberry Pi OS), reuse its
configuration. The image then uses the same central address and key, and
your devices and Home Assistant entities keep working without pairing
again.

**From the old system, bring:**

| | |
|---|---|
| hmbridge's `config.json` | central address, key or passphrase, devices, groups, MQTT. If you don't know where it is: `ps w \| grep hmbridge` on the running system shows `-config`. |
| `devices.json` from its `state_dir` | only if you paired devices *with hmbridge*; devices taken over from the CCU are in `config.json` |
| `system.key` | only if `security` uses a `key_file` |

You don't need `settings.json` or `state.json`; hmbridge rebuilds them. You
don't need anything from OpenCCU itself either: the module's firmware
lives on the module. Keep the old SD card as it is: it is your way back.

**Then:**

```sh
contrib/gokrazy/new-instance.sh ~/gokrazy/hmbridge     # the MQTT answers don't matter
cp config.json ~/gokrazy/hmbridge/hmbridge.json        # your old configuration
```

Change these fields in `hmbridge.json` and keep everything else:

| field | on OpenCCU | on gokrazy |
|---|---|---|
| `serial_port` | `/dev/raw-uart` | `/dev/ttyAMA0` |
| `reset` | `none` | `gpio` |
| `radio` | any | `auto` or the module's name (`rpi-rf-mod`, `hm-mod-rpi-pcb`); both work |
| `state_dir` | e.g. `/usr/local/hmbridge/state` | `/perm/hmbridge` |
| `require_synced_clock` | | `false` (see [Clock](#clock)) |
| `state_file`, `devices_file` | (deprecated) | remove |

For the key, choose one:

* **In the file** (`"security": {"key": "…"}` or `"passphrase"`). This is
  simplest. The key is then part of the image on the SD card, as it was on
  the OpenCCU card.
* **As a key file** (`"security": {"key_file": "/perm/hmbridge/system.key"}`).
  Copy the file to the Pi with the [debug image](#the-debug-image) before
  hmbridge starts with this configuration. As long as devices are
  configured, hmbridge refuses to create a new key: it stops instead of
  locking your devices out.

`devices.json`, if you have one, goes to `/perm/hmbridge/` the same way.

## Writing the SD card

**On Linux**, gok writes the card directly. Check the device name: the
whole card is overwritten.

```sh
gok -i hmbridge overwrite --full /dev/sdX
```

**On macOS** (or to keep an image file), build an image and write it with
`dd` or with [Raspberry Pi Imager](https://www.raspberrypi.com/software/)
("Use custom", and say **no** to its OS customisation):

```sh
gok -i hmbridge overwrite --full $HOME/hmbridge.img --target_storage_bytes=2000000000

diskutil list                          # find the card: "external, physical", the right size
diskutil unmountDisk /dev/disk4        # the whole disk (disk4), not a partition (disk4s1)
sudo dd if=$HOME/hmbridge.img of=/dev/rdisk4 bs=4m status=progress
diskutil eject /dev/disk4
rm $HOME/hmbridge.img                  # it contains your configuration
```

* Use `$HOME`, not `~`, after `if=`: zsh does not expand `~` there.
* `rdisk` is the raw device, much faster than `disk`.
* If macOS then says "The disk you inserted was not readable", click
  **Ignore**. It cannot read the image's ext4 and SquashFS partitions.
* A 2 GB image gives `/perm` about 800 MB, far more than hmbridge needs.
  Writing directly with gok sizes the partitions to the card instead.

macOS cannot create the ext4 file system for `/perm`. The image contains
gokrazy's [mkfs](https://github.com/gokrazy/mkfs) package, which creates
it on the Pi at the first boot.

## First boot

1. Put the card in the Pi. Connect Ethernet and, if you have one, an HDMI
   screen (it shows the kernel's messages). Power on. The red power LED
   stays off; the green LED blinks a heartbeat once the kernel runs (see
   [LEDs](#leds)).
2. The first boot creates `/perm` and **reboots once by itself**. The Pi is
   ready after about a minute.
3. Find the Pi. gokrazy sends its host name with DHCP, so `ping hmbridge`
   works if your router registers DHCP names (a FRITZ!Box does).
   Otherwise, look up its address in the router.
4. If the name does not resolve, put the address into `config.json` so
   that `gok` finds the Pi:

   ```json
   "Update": { "HTTPPassword": "…", "Hostname": "192.168.1.23" }
   ```

   Don't rename the instance directory for this: gok takes the address
   from `config.json`, and the directory name is only the instance name.
5. Open the web interface at `http://gokrazy:<Update.HTTPPassword>@hmbridge/`.
   It lists the services; `/user/hmbridge` must be running.
6. Watch hmbridge's log:

   ```sh
   gok -i hmbridge logs -s hmbridge
   ```

   A good start looks like this:

   ```text
   reset the radio module via GPIO 19 on /dev/gpiochip0 (RPI-RF-MOD reset line: the module answered IDENTIFY)
   uartgw: RPI-RF-MOD DualCoPro_App, firmware 4.4.22, serial …, SGTIN …
   radio module ready: RPI-RF-MOD, firmware 4.4.22, serial …, central address …
   state directory: /perm/hmbridge
   MQTT connected to tcp://…:1883
   ```

   On the very first start, a line `created /perm/hmbridge/system.key …`
   appears too (not with a key from your old installation).
   `connect: network is unreachable` right after the boot is normal: the
   network comes up a moment after hmbridge, which retries.
7. In Home Assistant, the hmbridge device appears (or your existing devices
   become available again). hmbridge's HTTP API answers at
   `http://hmbridge:8013/api/v1/health`.

## Operating it

**Log:** `gok -i hmbridge logs -s hmbridge`, or the service's page in the
web interface. gokrazy keeps the last 100 lines of each service.

**Updating hmbridge:**

```sh
git pull                              # this checkout (default)
gok -i hmbridge get                   # or: the newest release (instances made with -version)
gok -i hmbridge update
```

`gok update` builds the image, sends the new root file system and kernel
to the Pi, switches the active root partition, reboots, and waits until the
Pi answers again. `/perm` stays as it is. `gok -i hmbridge update
--testboot` asks for a test boot of the new partition instead of switching
to it directly (gokrazy's `gokrazy.try_boot_inactive`; see gokrazy's
documentation for its exact behaviour).

**Changing the configuration:** edit `hmbridge.json` (or `config.json`),
then run `gok -i hmbridge update`. The configuration is part of the image,
so there is nothing to edit on the Pi.

**Restarting or stopping hmbridge:** on its page in the web interface
(`/user/hmbridge`).

**Going back:** the previous system stays on the other root partition
until the next update. The usual way back is an update with the previous
version: check out the older commit, or, for an instance made with
`-version`, run `go get github.com/fjaeckel/hmgo/cmd/hmbridge@vX.Y.Z` in its
`builddir/github.com/fjaeckel/hmgo/cmd/hmbridge/`. Then run `gok -i
hmbridge update`. In an emergency, gokrazy's update API boots the previous
root partition directly. It keeps the boot partition (kernel,
`config.txt`) of the last update. This hasn't been tried on hardware:

```sh
curl -u gokrazy:<password> -X POST http://hmbridge/update/switch
curl -u gokrazy:<password> -X POST http://hmbridge/reboot
```

To return to OpenCCU, power off and put the old card back. The central address and key are the same, so your devices work
there as before. Only devices you newly paired on gokrazy are unknown to
OpenCCU. Never run both at once (with one module, that can't happen
anyway).

## The debug image

```sh
contrib/gokrazy/new-instance.sh -debug ~/gokrazy/hmbridge
```

Compared to the normal image, it adds:

* SSH access with a busybox shell, using gokrazy's
  [breakglass](https://github.com/gokrazy/breakglass) and your public key
  (`BREAKGLASS_KEYS`, or `~/.ssh/id_ed25519.pub`, `id_ecdsa.pub`,
  `id_rsa.pub`);
* `hmbridge -debug`: every radio frame in the log;
* the header's I²C bus (`dtparam=i2c_arm=on`), so the RPI-RF-MOD's clock
  chip (RX8130 at address 0x32) can be seen.

**breakglass does not start at boot.** That is gokrazy's design. Open
`/user/breakglass` in the web interface and press **restart**, then:

```sh
ssh hmbridge                                     # a busybox shell on the Pi
ssh hmbridge 'ls -l /dev/ttyAMA0 /perm/hmbridge/'
ssh hmbridge 'cat > /perm/hmbridge/system.key && chmod 600 /perm/hmbridge/system.key' < system.key
```

Copying a file with `ssh … 'cat > …'` works. hmbridge picks up a new key
file at its next start (it restarts by itself while the key is missing).

To turn a debug instance into a normal one, edit its `config.json`:

1. Remove `github.com/gokrazy/breakglass` and `github.com/gokrazy/serial-busybox` from `Packages`.
2. Remove breakglass's entry in `PackageConfig`.
3. Remove `-debug` from hmbridge's `CommandLineFlags`.
4. Run `gok -i hmbridge update`.

`/perm`, the key and the central address stay.

## Backing up the key

Without the AES system key, every AES device must be factory-reset and
paired again. If the key is in `hmbridge.json`, your instance directory is
the backup. If hmbridge created it on the Pi (`/perm/hmbridge/system.key`),
copy it with the debug image's SSH access:

```sh
ssh hmbridge 'cat /perm/hmbridge/system.key' > system.key
ssh hmbridge 'cat /perm/hmbridge/devices.json' > devices.json
```

Keep both somewhere safe. A normal image has no SSH: add breakglass for
the backup (as in the debug image), back up, and remove it again.

## How it works

### The image

| partition | content | on update |
|---|---|---|
| boot (FAT) | Raspberry Pi firmware, kernel, device trees and overlays, `config.txt`, `cmdline.txt` | replaced |
| root A / root B (SquashFS, read-only) | gokrazy, hmbridge, mkfs, `/etc/hmbridge/config.json` | the inactive one is written, then booted |
| `/perm` (ext4) | `/perm/hmbridge`: `system.key`, `devices.json`, `state.json`, `settings.json` | kept |

* **Kernel:** gokrazy's Raspberry Pi kernel (the Raspberry Pi Foundation's
  kernel). Every driver hmbridge needs is built in: the PL011 UART, the GPIO
  character device for the reset line, and, for the RPI-RF-MOD, I²C,
  `rtc-ds1307` (its RX8130 clock) and `leds-gpio`. No eQ-3 kernel modules
  are needed: hmbridge talks to the module directly.
* **Programs:** gokrazy's init, DHCP, NTP and web interface, `mkfs`, and
  hmbridge (started with `-config=/etc/hmbridge/config.json
  -create_key_file`). gokrazy restarts hmbridge whenever it exits (e.g.
  after a radio module error), which also resets the module again.

### The UART

The radio module needs the PL011 UART on GPIO 14/15 (`/dev/ttyAMA0`).
`new-instance.sh` writes these lines into `config.txt`
(`BootloaderExtraLines`):

```ini
[pi3]
dtoverlay=disable-bt
[pi4]
dtoverlay=disable-bt
[pi02]
dtoverlay=disable-bt
[pi5]
dtparam=uart0=on
[all]
dtparam=pwr_led_trigger=none
dtparam=act_led_trigger=heartbeat
```

On the Pi 3, 4 and Zero 2, the PL011 drives Bluetooth unless
`disable-bt` frees it; on the Pi 5, the header's UART0 must be switched
on. `"SerialConsole": "disabled"` keeps the kernel console off the UART
(gokrazy then sets `enable_uart=1` without `console=serial0`). gokrazy has
no udev, so there is no `/dev/serial0` symlink: the configuration names
`/dev/ttyAMA0`.

### LEDs

A Pi's red power LED is on whenever the Pi has power. In a box it looks
like an error and says nothing about hmbridge. The image switches it off
(`dtparam=pwr_led_trigger=none`) and lets the green activity LED blink a
heartbeat (`dtparam=act_led_trigger=heartbeat`):

| LEDs | meaning |
|---|---|
| green blinking (a double blink, about once a second) | the system runs |
| all off | no power, or the kernel has stopped |

With the power LED off, the Pi no longer shows a weak power supply on it.
The Pi still detects undervoltage itself (`/sys/class/hwmon/*/in0_lcrit_alarm`,
sensor `rpi_volt`). The Zero 2 W has no power LED. To get the power LED
back, remove the line from `BootloaderExtraLines` in `config.json` and run
`gok update`.

### Resetting the module

hmbridge resets the module through its reset line (GPIO 18 for the
HM-MOD-RPI-PCB, GPIO 19 for the RPI-RF-MOD), the way OpenCCU's raw-uart
driver does:

1. clear the UART input;
2. drive the line low for 50 ms, then high for 50 ms;
3. release it as an input.

The two modules have **opposite polarities**. The HM-MOD-RPI-PCB is reset
while the line is low; the RPI-RF-MOD while it is **high**. On a real
RPI-RF-MOD, holding the line high kept it silent, and releasing it low made
it announce its bootloader (`HMIP_TRX_Bl`) within 20 ms. The sequence
resets either module, and once the line is released, the module's own pull
resistor keeps it running. hmbridge then takes the RPI-RF-MOD from its
bootloader into its application (CHANGE_APP, `DualCoPro_App` about 2 s
later).

### Which reset line

With `"radio": "auto"`, hmbridge picks the line before the reset:

1. the RPI-RF-MOD's clock chip in sysfs, if a device tree overlay for it
   is loaded (not in this image): GPIO 19;
2. otherwise IDENTIFY: the RPI-RF-MOD answers it in its bootloader and in
   its application, so an answer means GPIO 19, and a classic answer GPIO 18;
3. no answer (a classic module, or an RPI-RF-MOD that hangs): hmbridge
   resets GPIO 19 and listens for 500 ms. An RPI-RF-MOD announces itself,
   which means GPIO 19; silence means GPIO 18.

The HM-MOD-RPI-PCB sits on header pins 1–12, so a reset on GPIO 19 (pin
35) does not affect it. With `"radio": "rpi-rf-mod"` or `"hm-mod-rpi-pcb"`,
hmbridge uses that module's line without asking.

### Clock

A Raspberry Pi has no real-time clock, and this image does not use the
RPI-RF-MOD's. gokrazy starts hmbridge only once the clock is set
(`WaitForClock` in `config.json`). The time comes first from the time saved
at the last shutdown, then from NTP (servers from DHCP, or the gokrazy
pool).

gokrazy's NTP client sets the clock without marking the kernel clock as
synchronised. hmbridge's `require_synced_clock` check would therefore never
pass, so it is `false` in `hmbridge.json`. The consequence: right after a
long power cut, the devices can get the saved, outdated time once. The next
time sync (`time_sync_interval`, hourly) corrects it.

### Version

`hmbridge -version`, `/api/v1/bridge` and the `hmbridge_build_info` metric
show the release version for instances made with `-version`, and `devel`
for a build from a local checkout.

## Troubleshooting

| symptom | cause | what to do |
|---|---|---|
| `dd: ~/hmbridge.img: No such file or directory` | zsh does not expand `~` after `if=`, or the image was not built yet | use `$HOME/hmbridge.img`; build it first |
| `lookup hmbridge: no such host` from gok | your network does not resolve the Pi's name | set `Update.Hostname` to its address in `config.json` (see [First boot](#first-boot)) |
| log: `the module did not answer IDENTIFY`, nothing received | the module does not get or answer the frames: wrong pins, UART still on Bluetooth, or an hmbridge older than the reset fix (it held an RPI-RF-MOD in reset) | update hmbridge; check the module's position; with the debug image, `ssh hmbridge 'cat /proc/device-tree/aliases/serial0'` must print `/soc/serial@7e201000` (the PL011) |
| hmbridge picks GPIO 18 for an RPI-RF-MOD | an hmbridge older than the detection fix | update, or set `"radio": "rpi-rf-mod"` |
| hmbridge restarts every few seconds | it exits on an error; gokrazy restarts it | read the log; the first lines of each attempt show the reason |
| log: `security.key_file … does not exist, but … devices are configured` | the key file is missing, and hmbridge refuses to replace it | copy the key to `/perm/hmbridge/system.key` (debug image), or put it into `hmbridge.json` |
| `ssh: connect to host … port 22: Connection refused` | breakglass is not started (it never starts at boot), or the image is not a debug image | start `/user/breakglass` in the web interface |
| devices "unavailable" right after a start | not heard since the start: window contacts send on a change or about hourly | wait, or trigger the device |
| macOS: "The disk you inserted was not readable" | macOS cannot read ext4/SquashFS | click Ignore |

## Tests

* **[vmtest/](vmtest/README.md)**, `make gokrazy-vmtest`: builds the image
  for QEMU and boots it with an emulated radio module and a broker. It pairs
  a device, updates over the network, and checks that the key and the
  devices survive. It also checks the Raspberry Pi image's boot files.
  Runs in CI.
* **[hwtest/](hwtest/README.md)**: tests a real Pi from your computer:
  bring-up, devices, pairing per model, reboots, updates, power cuts. It
  includes a runbook for the Raspberry Pi 3 B+ with an RPI-RF-MOD.

### Tested on hardware

Raspberry Pi 3 Model B Plus Rev 1.3 with an RPI-RF-MOD (firmware 4.4.22),
taken over from OpenCCU with 29 devices:

* boot, `/perm` creation, `config.txt` (PL011 on GPIO 14/15, Bluetooth off);
* reset on GPIO 19, bootloader, application;
* auto-detection with the module in its bootloader, in its application,
  and held in reset;
* MQTT, device state restored, devices heard, `gok update`.

Not yet tested on hardware: the HM-MOD-RPI-PCB with the current reset
sequence, the Pi 4, 5 and Zero 2 W.
