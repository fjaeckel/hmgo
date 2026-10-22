<!-- SPDX-License-Identifier: BSD-3-Clause -->

# Deploying hmbridge

There are two ways to set up hmbridge:

* **[Fresh installation (no OpenCCU)](#fresh-installation-no-openccu):** a
  new system on plain Raspberry Pi OS. hmbridge generates its own radio
  address and AES system key, and you pair your devices with it one by one.
  OpenCCU is never installed.
* **[Migrating from OpenCCU](#migrating-from-openccu):** hmbridge takes over
  an existing OpenCCU installation's radio address, key and devices, so
  nothing has to be paired again.

Sections 4 (Home Assistant), 5 (Adding devices) and 6 (Device settings)
apply to both.

## Fresh installation (no OpenCCU)

What you need: a Raspberry Pi 3 or 4 (a Pi 5 should work, see step 2), an
SD card, and one of eQ-3's radio boards on the Pi's GPIO header: an
**RPI-RF-MOD** (experimental support, see the README) or an
**HM-MOD-RPI-PCB**. Both sit on the first pins of the header (pin 1 at the
SD card end) and talk to the Pi's UART at 115200 baud, 8N1, without flow
control. Their reset lines are GPIO 18 (HM-MOD-RPI-PCB) and GPIO 19
(RPI-RF-MOD), both active low; hmbridge picks the right one (taken from
OpenCCU's device tree overlay).

What has been tested how:

| part | status |
|---|---|
| serial port setup (115200 8N1, raw, no flow control, non-blocking reads) | tested on a pseudo terminal with the emulated module (unit tests) |
| module detection after a reset, bootloader → application, firmware/serial query, `hmbridge probe` | tested with the emulated module (both kinds) |
| choosing the reset line (RTC present / module answers IDENTIFY) | tested on a pseudo terminal and with a fake sysfs |
| `hmbridge init`, key file, permissions, refusal to overwrite | tested |
| pulsing GPIO 19 on a real RPI-RF-MOD, GPIO chip lookup on a Pi 5 | **needs a hardware test** (GPIO 18 on the HM-MOD-RPI-PCB worked before) |
| RTC/LED overlay `contrib/raspberrypi/hmbridge-rpi-rf-mod.dts` | compiles; **needs a hardware test** |
| factory-reset procedures below | from eQ-3's manuals where marked; others **unverified** |

### Step 1: Raspberry Pi OS Lite

Write **Raspberry Pi OS Lite (64-bit)**, Bookworm or Trixie, to the SD card
with Raspberry Pi Imager. In its OS customisation set the host name (e.g.
`hmbridge`), a user, SSH, your time zone and, if needed, Wi-Fi. Boot the Pi,
log in with SSH and update it:

```sh
sudo apt update && sudo apt full-upgrade -y
```

### Step 2: UART for the radio module

The radio module needs the Pi's full UART (PL011, `/dev/ttyAMA0`) on GPIO
14/15, which Bluetooth uses by default on a Pi 3/4/Zero 2, and no serial
console on it.

1. Add to the end of `/boot/firmware/config.txt` (in the `[all]` section):

   ```ini
   enable_uart=1
   # Bluetooth off; the module gets the PL011 UART:
   dtoverlay=disable-bt
   # or, to keep (slow) Bluetooth on the mini UART instead:
   #dtoverlay=miniuart-bt
   ```

   With `disable-bt` also run `sudo systemctl disable --now hciuart`.
2. Remove the serial console: delete `console=serial0,115200` from
   `/boot/firmware/cmdline.txt` (the file is one single line; leave the rest
   unchanged). `sudo raspi-config` → Interface Options → Serial Port →
   "login shell: No", "serial port hardware: Yes" does steps 1 (without the
   Bluetooth overlay) and 2 as well.
3. `sudo reboot`, then check: `ls -l /dev/serial0` must point to `ttyAMA0`
   (not `ttyS0`, the mini UART), and nothing may use it
   (`sudo fuser /dev/ttyAMA0` prints nothing).

hmbridge uses `/dev/serial0` by default. **Pi 5 (unverified):** there
`serial0` is the debug connector; add `dtparam=uart0=on` instead of the
Bluetooth overlay (the Pi 5's Bluetooth has its own UART) and use
`"serial_port": "/dev/ttyAMA0"` (`hmbridge init -serial_port /dev/ttyAMA0`).

### Step 3 (RPI-RF-MOD, optional): RTC and status LEDs

The RPI-RF-MOD carries a real-time clock (Epson RX8130 at I²C address 0x32)
and an RGB status LED. hmbridge needs neither, but the RTC keeps the time
over a power cut without network, and with it hmbridge recognizes the
RPI-RF-MOD without asking it. Raspberry Pi's `i2c-rtc` overlay has no RX8130
option, so hmbridge brings its own overlay for the **Raspberry Pi 3 and 4**
(**not verified on hardware yet**; Pi 5: see the note below):

```sh
sudo apt install device-tree-compiler
curl -LO https://raw.githubusercontent.com/fjaeckel/hmgo/master/contrib/raspberrypi/hmbridge-rpi-rf-mod.dts
sudo dtc -@ -I dts -O dtb -o /boot/firmware/overlays/hmbridge-rpi-rf-mod.dtbo hmbridge-rpi-rf-mod.dts
printf 'dtparam=i2c_arm=on\ndtoverlay=hmbridge-rpi-rf-mod\n' | sudo tee -a /boot/firmware/config.txt
sudo reboot
```

Then `cat /sys/bus/i2c/devices/1-0032/name` shows `rx8130`, `/dev/rtc0`
exists and `sudo hwclock -r` (package `util-linux-extra`) reads the clock.
To have the clock set from the RTC at boot instead of from the last
shutdown, `sudo apt purge fake-hwclock`. The LEDs appear as
`/sys/class/leds/rpi_rf_mod:{red,green,blue}`.

**Pi 5 (unverified):** the header's GPIOs are on the RP1 chip there
(`&rp1_gpio`), not on the SoC's `&gpio`, which the overlay's LED part uses:
on a Pi 5 it would drive SoC pins instead of the module's LEDs. Use the
overlay on a Pi 5 only after replacing `&gpio` by `&rp1_gpio` in its LED
fragment (`sed -i 's/<&gpio /<\&rp1_gpio /' hmbridge-rpi-rf-mod.dts`), or
delete that fragment and keep the RTC part. hmbridge still hands out the
time to the devices only once NTP has synchronised the clock
(`require_synced_clock`).

### Step 4: User

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin --groups dialout,gpio hmbridge
```

`dialout` gives access to the serial port, `gpio` to the reset line
(`/dev/gpiochip*`).

### Step 5: Install hmbridge

Take the binary from a [release](https://github.com/fjaeckel/hmgo/releases):
a tagged release `v…`, or `nightly`, the latest build of the `master`
branch (not a stable release). Each release has the binaries for all architectures
(`hmbridge-<version>-linux-arm64` for 64-bit Raspberry Pi OS) and a checksum
file `SHA256SUMS-<version>`; the binaries carry a signed build provenance
attestation from GitHub Actions.

With the [GitHub CLI](https://cli.github.com/) (`gh`, logged in with
`gh auth login`; you can also do this on your desktop and copy the file):

```sh
TAG=v0.1.0   # or: nightly
gh release download "$TAG" -R fjaeckel/hmgo -p 'hmbridge-*-linux-arm64' -p 'SHA256SUMS-*'
sha256sum --ignore-missing -c SHA256SUMS-*      # must print "OK"
gh attestation verify hmbridge-*-linux-arm64 -R fjaeckel/hmgo
sudo install -m 0755 hmbridge-*-linux-arm64 /usr/local/bin/hmbridge
hmbridge -version
```

Without `gh`, download the same two files from the release page (or with
`curl -LO https://github.com/fjaeckel/hmgo/releases/download/<tag>/<file>`)
and check them with `sha256sum`. The checksum only protects against a
damaged download; the attestation proves that the binary was built by this
repository's workflow.

### Step 6: Check the radio module

```sh
sudo hmbridge probe
```

opens `/dev/serial0`, resets the module, identifies it and prints e.g.

```text
serial port:    /dev/serial0 -> /dev/ttyAMA0
reset:          GPIO 19 on /dev/gpiochip0 (RPI-RF-MOD reset line: the module answered IDENTIFY)
module:         RPI-RF-MOD
application:    DualCoPro_App
firmware:       4.4.22
serial number:  …
SGTIN:          …
OK: the radio module works. Next: hmbridge init (see docs/DEPLOY.md).
```

It needs no configuration (flags `-serial_port`, `-radio`, `-reset`,
`-reset_gpio`, `-debug`; `hmbridge -probe` does the same with the settings
of an existing configuration). Run it again as the service user to check the
permissions: `sudo -u hmbridge hmbridge probe`. If it fails, it prints a hint;
the usual causes are a serial console or Bluetooth still on the UART
(step 2), or a module not seated on the right pins. `-debug` shows every
frame.

### Step 7: Configuration and system key

```sh
sudo hmbridge init
```

asks for the MQTT broker (e.g. `tcp://homeassistant.local:1883`), the MQTT
user and password (not echoed), the serial port and the central address, and
creates:

| file | content | owner, mode |
|---|---|---|
| `/etc/hmbridge/config.json` | configuration: radio `auto`, `reset` `gpio`, a random `central_address`, `security` with `key_file` and `index` 1, MQTT, `state_dir`, no devices | `root:hmbridge`, 0640 (it holds the MQTT password) |
| `/var/lib/hmbridge/` | `state_dir` | `hmbridge`, 0700 |
| `/var/lib/hmbridge/system.key` | a new random 128-bit AES system key (32 hex digits) | `hmbridge`, 0600 |

The key is never printed and never written into `config.json`. **Back up
`system.key`** (and later `devices.json`) somewhere safe: without it, every
device that got the key while pairing must be factory-reset and paired
again. hmbridge warns (log, `GET /api/v1/health`, `-check_config`) if the key file is
accessible by its group or other users or belongs to another user than root
or `hmbridge`, and refuses a key file that is a symbolic link, FIFO or device
(name the file itself). With `-key_file` in a directory that does not exist
yet, `hmbridge init` creates it `root:hmbridge` 0750. `hmbridge init` never
overwrites an existing configuration or key file.

For scripts: `hmbridge init -non_interactive -mqtt_broker tcp://ha.lan:1883
-mqtt_username hmbridge -mqtt_password_file /root/mqtt-pw` (`-` reads the
password from stdin; `hmbridge init -h` lists all flags, e.g. `-config`,
`-state_dir`, `-serial_port`, `-central_address`).

The **central address** is hmbridge's radio address (24 bits). A new
installation needs one no device knows yet; `init` picks a random one
between `100000` and `EFFFFF`. `000000` (no central / unpaired) and `FFFFFF`
are reserved, and as a precaution the ranges below `100000` and above
`EFFFFF` are avoided too (other software around the CCU uses low and high
addresses for broadcasts and virtual devices; this is a convention, not an
eQ-3 rule). A clash with a device's factory address is very unlikely, and
pairing refuses a device that has the central's address.

Check it as the service user:

```sh
sudo -u hmbridge hmbridge -check_config
```

### Step 8: systemd

```sh
curl -LO https://raw.githubusercontent.com/fjaeckel/hmgo/master/contrib/systemd/hmbridge.service
sudo install -m 0644 hmbridge.service /etc/systemd/system/hmbridge.service
sudo systemctl daemon-reload
sudo systemctl enable systemd-time-wait-sync   # start only with a synchronised clock
sudo systemctl enable --now hmbridge
journalctl -u hmbridge -f
```

The unit runs hmbridge as `hmbridge` in a sandbox with access only to the
serial port, the GPIO chips, `/etc/hmbridge` (read-only) and
`/var/lib/hmbridge`. The log shows `radio module ready: …` and `MQTT
connected to …`. The read-only [HTTP API](#http-api) and `/metrics` are on
port 8013; `curl -s localhost:8013/api/v1/health` should say `"status": "ok"`
once MQTT is connected.

### Step 9: MQTT in Home Assistant

1. Install the **Mosquitto broker** add-on (Settings → Add-ons), or use your
   own broker.
2. Create a Home Assistant user for hmbridge (Settings → People → Users,
   e.g. `hmbridge`); the Mosquitto add-on accepts Home Assistant users as
   MQTT logins. Use these credentials in `hmbridge init`.
3. Add the **MQTT** integration (Settings → Devices & services) if Home
   Assistant did not offer it already.

The device **hmbridge** appears with the button **Pair new device** and the
pairing sensors (see [section 4](#4-home-assistant)).

### Step 10: Pair the devices one by one

Pair one device at a time, as described in
[section 5](#5-adding-devices): press **Pair new device**, put the device into
pairing mode (its config button, see its manual), wait for "Last pairing
result", rename the new device in Home Assistant, then go on with the next.
Read the warning about the **key hand-out** in section 5 first: every AES
device receives the new system key encrypted with eQ-3's public default key,
so pair where nobody suspicious is within radio range and check the serial
number in each result.

Factory-new devices pair right away. A device that was paired to another
central (a CCU, OpenCCU, FHEM, Homegear) refuses with `… is paired to
another central`: **factory-reset it first**. A device that was paired with
hmbridge before and unpaired keeps hmbridge's system key; pairing it again
needs no reset (hmbridge retries the key exchange with the system key, then
with the previous key).

**Factory reset of devices with a foreign system key.** eQ-3's manuals warn
that a device which still has a system key set by its central refuses the
reset at the device: its LED lights steadily instead of blinking after the
first long press. Such a device has to be reset by its old central: delete
it there (OpenCCU/CCU WebUI: Settings → Devices → Delete, with "reset to
factory settings" ticked) while the central still runs. If that central is
gone, you need its system key: take over its radio address and key instead
(the migration path below). Devices that never had a system key set reset
fine.

Factory reset per model:

| model | factory reset | source |
|---|---|---|
| HM-CC-RT-DN (radiator thermostat) | Remove the batteries. Hold the three buttons (Auto/Manu, Boost, Comfort/Eco) while inserting the batteries again, release when `rES` appears. Or: hold Auto/Manu > 3 s, choose `rES` with the wheel, confirm with Boost, turn to `YES`, confirm with Boost. | eQ-3 manual |
| HM-TC-IT-WM-W-EU (wall thermostat) | Remove the batteries, hold the three buttons while inserting them again, release when `rES` appears; or via the configuration menu (`rES`). | eQ-3 manual |
| HM-RC-8 (remote) | Hold the teach-in button (back, under the cover) ≥ 5 s until the LED blinks red slowly, then hold it again ≥ 5 s (the LED blinks faster) and release; the remote restarts (LED test). A steady LED instead: still bound to a central with a system key (see above). | eQ-3 manual |
| HM-LC-Sw1-Pl-DN-R1 (switch plug) | Hold the channel button ≥ 4 s until the LED blinks slowly, then hold it again ≥ 4 s until the LED blinks fast. | eQ-3 manual (first step); second step **unverified** |
| HM-Sec-SCo (window contact) | Hold the teach-in button (behind the LED, use a paper clip) until the LED blinks red, then hold it again ≥ 5 s (blinks faster); the LED lights red for about 3 s when done. | third-party guide, **unverified** |
| HM-Sec-SC-2, HM-Sec-SC (window contacts), HM-Sec-RHS (window handle), HM-PB-2-WM55 (wall switch), HM-WDS10-TH-O, HM-WDS30-T-O (outdoor sensors) | Presumably eQ-3's usual procedure for battery devices: hold the teach-in/config button ≥ 4–5 s until the LED blinks red slowly, then hold it again ≥ 4–5 s until it blinks faster, release. Check the device's manual. | **unverified** |
| HM-LC-Dim1L-Pl-3 (dimmer plug), HM-OU-LED16 (LED display) | Presumably as the switch plug: hold the channel/config button ≥ 4 s until the LED blinks slowly, then again ≥ 4 s until it blinks fast. Check the device's manual. | **unverified** |

### Step 11: Heating groups

A heating group combines thermostats and window contacts of a room into one
Home Assistant climate entity that sets all members at once. Groups are
defined in `config.json`, and their members must be configured there too:
after pairing, copy each member's entry (name, model, address, serial,
`aes_channels`, `key_index`) from `/var/lib/hmbridge/devices.json` into
`devices` in `config.json` (see section 5), add the group, and restart:

```json
"groups": [ { "name": "Bad", "members": ["Bad Heizung", "Bad Fenster"] } ]
```

Such a group is driven by hmbridge. Direct radio links between the devices
themselves (a window contact lowering a radiator thermostat, a wall
thermostat controlling the radiator thermostats of its room) need peering,
which hmbridge does not set up yet; support is being added, see the section
*Direct links* once it is available.

## Migrating from OpenCCU

hmbridge needs exclusive access to the radio module (HM-MOD-RPI-PCB or
RPI-RF-MOD). While
you are still evaluating it, you keep OpenCCU installed and take turns: stop
OpenCCU's radio services (or boot a different SD card), run hmbridge, and go
back to OpenCCU whenever you like. Your thermostats keep their pairing and
their own week programs either way, because hmbridge presents itself with
OpenCCU's radio address and AES key.

## 1. Collect what hmbridge needs from OpenCCU

Enable SSH in OpenCCU (Settings → Control panel → Security), log in as root,
and run:

```sh
# Radio module firmware: hmbridge needs the classic 1.x firmware.
cat /var/rf_firmware_version 2>/dev/null
grep -i -E "copro|firmware" /var/log/messages | tail -n 5

# The CCU's own BidCoS radio address ("central_address" in the config).
cat /etc/config/ids

# Device radio addresses, types and serial numbers.
ls /etc/config/rfd/
```

`/var/hm_mode` tells you which radio board you have (`HM_HMRF_DEV`):
`HM-MOD-RPI-PCB` (classic firmware 1.x) or `RPI-RF-MOD` (eQ-3 dual firmware,
e.g. 4.4.22). hmbridge supports both and detects them; RPI-RF-MOD support is
experimental (see the README).

If you set a "system security key" in OpenCCU, put that passphrase into
`security.passphrase`. Its index is 1 if you set the key once, and goes up by
one every time it was changed. If you never set one, leave `security` out.

If the key was changed but some devices were never switched to the new key
(e.g. they were out of reach), put the old passphrase into
`security.previous` and give those devices `"key_index"` with the old index.
hmbridge logs a hint when a device's messages fail AES authentication.

If AES fails for a device ("message failed AES authentication" in the
log), find out which key it really uses with `hmkeycheck`. It checks
candidate keys offline against the AES handshakes recorded in an
`hmbridge -debug` log and never prints key material:

```sh
hmkeycheck -keys /etc/config/keys hmbridge-test.log   # values from OpenCCU's key file
hmkeycheck -stdin hmbridge-test.log                    # type passphrases, one per line, then Ctrl-D
```

The eQ-3 default key is always tried as well.

## 2. Configuration

The easiest way is to let `hmgenconfig` write the configuration from your
OpenCCU (next section). You can also write it by hand: copy
`internal/config/example.json` to `/etc/hmbridge/config.json` and fill in
`central_address`, the MQTT broker of your Home Assistant installation and one
entry per device.

### Generating the configuration

`hmgenconfig` runs on OpenCCU's Pi **while OpenCCU is running normally** and
prints a ready-to-use hmbridge configuration with all your BidCoS devices,
their names, addresses, AES settings and heating groups.

It only reads; nothing on OpenCCU is changed. It never reads key material
(`/etc/config/keys` or similar), so the security passphrase is left for you to
fill in. Its sources, each optional:

| Source | What it provides | Flag to change / disable (`""`) |
| --- | --- | --- |
| `/etc/config/ids` | `central_address` (`BidCoS-Address`, converted to hex) | `-ids` |
| `/etc/config/rfd/*.dev` | address, model, serial and AES key index of every device | `-rfd` |
| rfd's XML-RPC API, `http://127.0.0.1:2001/` (`listDevices`) | which channels have AES ("secured" transmission mode); also devices and central address if the files are missing | `-xmlrpc` |
| ReGaHss, `http://127.0.0.1:8183/rega.exe` (a read-only script) | the device names you gave in the WebUI | `-rega` |
| `/etc/config/groups.gson` | the heating groups (`HomeMatic.heating`) | `-groups` |
| `/var/hm_mode` | the radio module (`radio`) | `-hm_mode` |

The names and AES settings need OpenCCU's services: if rfd or ReGaHss is not
running (e.g. you already stopped them for hmbridge), `hmgenconfig` still
writes a valid configuration, but with generated names (the group name plus
the model, or the model plus the serial) and every device's AES state marked
"unknown" (`aes` off). It tells you on stderr what it used and what was
missing.

Copy the binary (`hmgenconfig-…-linux-arm64` for OpenCCU on a Pi 3/4/5,
`…-linux-armv7` for a 32-bit system) to the Pi and run it as root:

```sh
scp dist/hmgenconfig-*-linux-arm64 root@openccu:/usr/local/hmgenconfig
ssh root@openccu
cd /usr/local
./hmgenconfig > config.json          # or: ./hmgenconfig -out config.json
```

If something looks wrong, run `./hmgenconfig -dump > config.json 2> dump.txt`:
`dump.txt` then contains the raw inputs (first line of every `.dev` file,
the `listDevices` summary, the ReGaHss output and `groups.gson`; no keys), which
helps when reporting a problem.

Then edit `config.json`:

1. `security.passphrase`: replace `CHANGE-ME` with OpenCCU's system security
   key. `security.index` is the key index most of your devices use (from the
   `.dev` files). If some AES devices still use an older index, there is a
   `security.previous` block with `CHANGE-ME-PREVIOUS` and those devices carry
   `"key_index"`; enter the old passphrase, or re-key the devices in OpenCCU
   and remove both. If all devices use the default key (index 0), there is no
   `security` block.
2. `mqtt`: the broker URL, user name and password of your Home Assistant MQTT
   broker.

hmbridge refuses to start while any value still starts with `CHANGE-ME`
(`-check_config` reports which one), so the unedited output of hmgenconfig is
expected to fail the check.

What else to look at before the first run:

* `devices` are sorted by group, then by name. `aes` is set when OpenCCU
  reports AES active on any channel; `aes_channels` only appears when just some
  channels are secured (typically `[2]` on a wall thermostat). Device names
  must be unique: duplicates get the serial appended. A `_comment` on a device
  says when its name was generated or its AES state is unknown.
* `groups` mirror OpenCCU's heating groups; members refer to device names.
  Groups without a supported thermostat are left out, and a device can only be
  in one group.
* Devices hmbridge does not support yet (e.g. motion detectors) are not in
  `devices`; they are listed in the top-level `_comment` as `not supported
  yet: <name> (<model> <address>)`. OpenCCU's own radio (HM-RCV-50) is listed
  as `not a device`. The weather sensors have no AES, so they carry neither
  `aes` nor an "AES state unknown" note.
* `serial_port` is `/dev/raw-uart` with `reset` `none`, and `state_dir` is
  `/usr/local/hmbridge/state` (OpenCCU's writable partition), which fits
  running hmbridge on OpenCCU's Pi (section 3a). For Raspberry Pi OS (section
  3b), remove those three lines to get the defaults.
* To start small, delete devices and groups you don't want yet (and remove
  deleted devices from the groups' `members`).

Finally check the result (it fails, naming the setting, while a `CHANGE-ME`
placeholder is left):

```sh
./hmbridge -config config.json -check_config
```

## 3a. Running on OpenCCU's Pi without reinstalling (experimental)

Build the binaries with `make dist` (or take them from the CI artifacts)
and copy the matching one to the Pi: `linux-armv7` for a 32-bit OS on a
Pi 2/3/4, `linux-arm64` for a 64-bit OS, `linux-armv6` for a Pi 1/Zero. Check
the configuration with `hmbridge -config … -check_config`, then stop the services that own the radio module. Service names
differ between OpenCCU releases, so look them up first:

```sh
ls /etc/init.d/ | grep -i -E "multimac|rfd|hmip|hs485"
monit summary 2>/dev/null   # OpenCCU restarts crashed services via monit
```

Stop monit's supervision of those services and the services themselves, then
run hmbridge with `"serial_port": "/dev/raw-uart"`, `"reset": "none"` and a
writable `"state_dir"` such as `"/usr/local/hmbridge/state"` (OpenCCU's root
file system is read-only and `/var` is lost on reboot; see section 4,
*Persistence*). To return to OpenCCU, simply reboot the Pi.

## 3b. Running on Raspberry Pi OS (final setup)

On a Raspberry Pi OS Lite installation:

1. `/boot/firmware/config.txt`: `enable_uart=1` and `dtoverlay=disable-bt`
   (Pi 3/4/Zero 2: gives the module the full PL011 UART on GPIO 14/15).
2. Remove `console=serial0,115200` from `/boot/firmware/cmdline.txt` and run
   `sudo systemctl disable --now hciuart`.
3. Create the user:
   `sudo useradd --system --no-create-home --groups dialout,gpio hmbridge`.
   Install the binary to `/usr/local/bin/hmbridge`, the config readable only
   by root and hmbridge (it holds the AES system key and the MQTT password):
   `sudo install -d -m 0750 -o root -g hmbridge /etc/hmbridge` and
   `sudo install -m 0640 -o root -g hmbridge config.json /etc/hmbridge/config.json`,
   and `contrib/systemd/hmbridge.service` to `/etc/systemd/system/`. hmbridge
   warns (log, `GET /api/v1/health`, `-check_config`, metric
   `hmbridge_config_insecure_permissions`) if the config file is readable
   by other users or groups, writable by its group, or owned by another user.
4. A Raspberry Pi has no battery-backed clock. Make sure NTP works
   (`timedatectl` shows "System clock synchronized: yes"); hmbridge only hands
   out the time to the devices once it is. Optionally
   `sudo systemctl enable systemd-time-wait-sync` so that services ordered
   after `time-sync.target` (like hmbridge) start only with a synchronised
   clock.
5. `sudo systemctl enable --now hmbridge` and watch
   `journalctl -u hmbridge -f`.

Keep the default `"serial_port": "/dev/serial0"` and `"reset": "gpio"`:
hmbridge pulses the module's reset line, GPIO 18 on the HM-MOD-RPI-PCB and
GPIO 19 on the RPI-RF-MOD (with `"radio": "auto"` it finds out which module
is attached first; `reset_gpio` overrides the line). The steps of the
[fresh installation](#fresh-installation-no-openccu) (UART, user, binary,
`hmbridge probe`, systemd) apply here as well.

The [HTTP API](#http-api) and `/metrics` listen on all interfaces (`:8013`)
so that Prometheus and monitoring can reach them. They are read-only (only
`GET` and `HEAD`; everything else gets "405 Method Not Allowed") and contain
no keys or passwords, but they are **not authenticated** and not harmless:
anyone who reaches the port sees every device's name, model, address,
serial number, state and battery level (which windows are open, whether
someone is home), the device settings, the radio module's serial number and
SGTIN, the central address, the `state_dir` path and, in `warnings`, the
paths of the configuration and key files (e.g. when their permissions are
too open). Set `"http_listen": "127.0.0.1:8013"` to keep them local (and
scrape through an SSH tunnel or a local Prometheus agent), or allow port
8013 only from your monitoring host in the firewall, e.g.
`nft add rule inet filter input tcp dport 8013 ip saddr != 192.168.1.10 drop`.

Requests must name hmbridge in their `Host` header: an IP address,
`localhost`, the machine's host name (also `<name>.local`), or a name listed
in `"http_allowed_hosts": ["hmbridge.example.lan"]` (e.g. behind a reverse
proxy; `"*"` accepts any name). Other names get "421 Misdirected Request".
This stops DNS rebinding, where a web site you visit re-points its own name
to hmbridge's address to read the API from your browser.

### HTTP API

hmbridge has no web UI. Port 8013 serves a small **read-only** JSON API for
monitoring and scripts; pairing, unpairing, renaming and device settings are
done in Home Assistant (MQTT) only. All responses are JSON
(`Content-Type: application/json`, field names in `snake_case`, times in
RFC 3339); errors are `{"error": "…"}` with 404 (unknown path or device) or
405 (any method but `GET`/`HEAD`, with `Allow: GET, HEAD`). No response ever
contains key material: devices show their AES channels and key index only.
There is no authentication: what it exposes (device serials and states, the
module's serial and SGTIN, `state_dir`, file paths in `warnings`) is
readable by everyone who reaches the port; restrict it as described in
[3b](#3b-running-on-raspberry-pi-os-final-setup) (`"http_listen": "127.0.0.1:8013"` or a
firewall rule).

| endpoint | content |
|---|---|
| `GET /` | index of the endpoints |
| `GET /api/v1/health` | `status` (`ok`, `degraded`, `down`), `radio` (`ok`, `kind`, `last_error`), `mqtt` (`enabled`, `connected`), `clock_synced`, `clock_sync_required`, `warnings`. HTTP 200 for `ok` and `degraded`, **503 for `down`** |
| `GET /api/v1/bridge` | `version`, `go_version`, `started`, `uptime_seconds`, `module` (`kind`, `firmware`, `serial`, `sgtin`), `central_address`, `state_dir`, `pairing` (`enabled`, `disabled_reason`, `active`, `status`, `remaining_seconds`, `aes`, `last_result`, `last_result_at`, `unpair_waiting`), `devices` (count), `command_queue`, `warnings` (insecure configuration permissions, `devices.json` problems) |
| `GET /api/v1/devices` | list of devices, each as below |
| `GET /api/v1/devices/{address}` | one device (address: 6 hex digits, any case; 404 if unknown): `name`, `model`, `address`, `serial`, `source` (`config` or `paired`), `group`, `available`, `last_seen`, `rssi`, `messages_received`, `aes` (`enabled`, `channels`, `key_index`), `last_event`, `summary`, `last_command`, `state` (exactly as published to Home Assistant on `hmbridge/<address>/state`), `settings` (as published on `hmbridge/<address>/settings`: `current`, `status`, `detail` per changed setting, `read_at`; `null` for models without settings) |
| `GET /api/v1/groups` | heating groups: `name`, `slug`, `members`, `state` |
| `GET /api/v1/unknown` | `unknown_senders` (radio addresses that sent to the central but are not configured: `address`, `messages`, `last_seen`, `last_cmd`, `rssi`, `serial`, `model_id`; at most 100) and `hmip_heard` (Homematic IP devices the RPI-RF-MOD heard, [HMIP.md](HMIP.md)) |
| `GET /metrics` | Prometheus metrics |

`status` is `down` when the radio module failed (hmbridge then exits and
systemd restarts it), `degraded` while MQTT is enabled but not connected or
the system clock is not synchronised although `require_synced_clock` is set.
`warnings` lists those reasons and configuration problems; the latter do not
change `status`.

```sh
# Health check (exit status 0 unless down), e.g. for a monitoring system:
curl -fsS http://hmbridge.lan:8013/api/v1/health | jq -r .status

# Battery and signal of all devices:
curl -s http://hmbridge.lan:8013/api/v1/devices |
  jq -r '.[] | [.name, .state.battery_voltage, .state.low_battery, .rssi, .last_seen] | @tsv'

# One device's settings and the result of the last command:
curl -s http://hmbridge.lan:8013/api/v1/devices/38f59c | jq '{last_command, settings}'

# Addresses of devices that talk to the central but are not configured:
curl -s http://hmbridge.lan:8013/api/v1/unknown | jq -r '.unknown_senders[] | "\(.address) \(.serial) \(.model_id)"'
```

The former status page is gone. `GET /api/state` (its JSON) still answers
for one more release, marked with a `Deprecation: true` header; move to
`/api/v1`.

## 4. Home Assistant

Make sure the MQTT integration is set up in Home Assistant. Every configured
device appears as a Home Assistant device as soon as hmbridge connects:

| model | entities |
|---|---|
| HM-CC-RT-DN | climate (auto/heat/off, preset boost); sensors temperature, target temperature, valve position, battery voltage and estimate; battery low; diagnostic: control mode, boost remaining, fault, last command |
| HM-TC-IT-WM-W-EU | as the HM-CC-RT-DN, plus humidity and window, without valve and fault |
| HM-Sec-SCo, HM-Sec-SC-2, HM-Sec-SC | window, battery low, sabotage |
| HM-Sec-RHS | as above, plus window state (closed/tilted/open) |
| HM-LC-Sw1-Pl-DN-R1 | switch (named like the device); diagnostic: last command |
| HM-LC-Dim1L-Pl-3 | light with brightness and transition (the transition becomes the dimmer's ramp time, default 0.5 s); diagnostic: fault (`load_failure`), last command |
| HM-PB-2-WM55, HM-RC-8 | per button an event entity "Button n" with the event types `press_short`, `press_long`, `press_long_release`, and device triggers ("Button n" short press / long press / long release) for automations; battery low |
| HM-OU-LED16 | per LED a select "LED n" (off, red, green, orange); diagnostic: last command |
| HM-WDS10-TH-O | temperature, humidity, battery low |
| HM-WDS30-T-O | temperature, battery low |

All devices also have the diagnostic sensors last seen, RSSI and messages
received (the last two disabled by default) and, except the weather sensors,
AES authentication.
Devices paired with hmbridge (section 5) additionally have a "Name" text
entity and an "Unpair" button (disabled by default). Devices with settings
(section 6) have configuration entities, a "Settings" status sensor and a
"Refresh settings" button. hmbridge itself appears
as the device "hmbridge" with the "Pair new device" button and the pairing
sensors.

Notes per device type:

* **Switch, dimmer, LED display** are asked for their status shortly after
  hmbridge starts and then every `status_poll_interval` (default 1 hour), so
  Home Assistant knows their state and whether they are reachable. Of the LED
  display only LED 1 is asked (asking all 16 would block the radio for long
  if the display is unreachable); the other LEDs show the last color set by
  Home Assistant or reported by the display, also across restarts. A command
  the device does not answer is retried twice, then "Last command" shows
  `failed: … (… did not respond)`.
* **Remotes** report only button presses. A short press is `press_short`; a
  held button gives `press_long` at once and `press_long_release` when it is
  let go. If a button's channel uses AES, hmbridge only accepts authenticated
  presses; the repeats sent while a button is held cannot be authenticated,
  so on AES buttons `press_long` arrives together with `press_long_release`
  (dimming while holding a button needs a button without AES). If the
  release is never received, hmbridge sends `press_long_release` 5 seconds
  after the last repeat. Presses are events, not state: nothing about them is
  retained or saved. Use the device triggers or the event entities in
  automations.
* **Weather sensors** send every few minutes.

A device that stops reporting (empty battery, out of range) becomes
unavailable in Home Assistant after its `stale_after` time: 30 minutes for
thermostats, 1 hour for weather sensors, 6 hours for the HM-Sec-SCo, 50
hours for other contacts, three status poll intervals (3 h 5 min) for
switches, dimmers and the LED display, and never for remotes (they are
silent until a button is pressed; set `stale_after` to change that). Its
"Last seen" sensor stays visible. Actuators are only heard from when polled,
so their `stale_after` must be at least `status_poll_interval` plus 5 minutes.

After a restart or reboot, hmbridge reads its last published state back from
the MQTT broker, so Home Assistant isn't blank until every window contact has
reported again; this needs no writes on the Pi.

**Persistence.** hmbridge keeps its own data as small JSON files in one
directory, `state_dir` (default `/var/lib/hmbridge`; the systemd unit creates
it with `StateDirectory=hmbridge`, mode 0700; on OpenCCU use
`/usr/local/hmbridge/state`):

| file | content | written |
|---|---|---|
| `state.json` | last known device state, the fallback when the broker is unreachable at startup | after significant changes (mode, target temperature, window, battery, fault, switch/dimmer level, LED colors), at most every `state_save_interval` (15 min), and on shutdown; ordinary temperature reports never cause a write |
| `devices.json` | devices paired with hmbridge (section 5) | on pair, unpair and rename |
| `settings.json` | the device settings last read from the devices (section 6); losing it only means reading them again | when the settings read from a device change |

To spare the SD card, a file is never rewritten with unchanged content.
Writes are atomic (a power cut leaves the old or the new version), files are
readable only by the hmbridge user (0600), and symbolic links are not
followed. If a file is damaged, hmbridge renames it to
`<name>.corrupt-<time>` (keeping the newest three), logs it, and starts
without it; for `devices.json` `GET /api/v1/bridge` shows a warning — repair the
copy and put it back while hmbridge is stopped. A `devices.json` written by a
newer hmbridge (after a downgrade) is left untouched and pairing is disabled
until you upgrade again or remove it. `"state_dir": ""` disables all files
(and pairing). Back up `devices.json` together with `config.json`.

*Upgrading:* the older options `state_file` and `devices_file` still work
(they name exact paths and are logged as deprecated). When you set a
`state_dir` other than `/var/lib/hmbridge`, hmbridge copies
`/var/lib/hmbridge/state.json` and `devices.json` into it on the first start
if they are not there yet, and leaves the old files in place. Files from
other locations (e.g. `/usr/local/hmbridge/devices.json` from an older
OpenCCU setup) are not copied: move them into `state_dir` yourself, while
hmbridge is stopped, then remove `devices_file` from the configuration.

`GET /api/v1/devices` on port 8013 lists the configured and paired devices,
`GET /api/v1/unknown` every radio address that talked to the central but is
not configured — handy for finding device addresses ([HTTP API](#http-api)).

## 5. Adding devices

hmbridge can pair (teach in) new classic HomeMatic devices itself, so OpenCCU
is no longer needed for that. Supported are the models in the
[README](../README.md#supported-devices).

1. In Home Assistant, open the device **hmbridge** and press **Pair new
   device** (pairing is only started from Home Assistant or MQTT; the HTTP API
   is read-only, `GET /api/v1/bridge` shows the pairing state). Pairing mode
   stays on for 60 seconds (`pairing.duration`); the sensors "Pairing mode",
   "Pairing time remaining" and "Last pairing result" show what happens.
   Anyone who can publish to the MQTT topic `hmbridge/bridge/set/pair` (any
   MQTT user with write access to `hmbridge/#`) can start pairing mode too:
   give MQTT write access only to Home Assistant.
2. Put the device into pairing mode: press its config button (usually a short
   press; the LED blinks), as described in its manual. Battery devices stay
   awake while their LED blinks.
3. hmbridge recognises the model from the device's announcement (DEVICE_INFO),
   writes your `central_address` into the device, and, with a `security` block,
   gives it your system security key and switches AES on where a CCU would
   (window contacts, push buttons, remotes). "Last pairing result" then says
   e.g. `paired HM-Sec-SCo NEQ1234567 (4c5d70, …, AES on channels [1] with key
   index 2)`, and the device appears in Home Assistant as "HM-Sec-SCo
   NEQ1234567". Rename it there (HA's device name) or with its "Name" entity,
   which also renames it in hmbridge's logs and metrics.

Pairing mode ends after one device (one at a time) or when the time is up.

**Pairing hands out your system security key.** With `pairing.aes`
`"system_key"` (the default with a `security` block, like OpenCCU), the
device that announces itself while pairing mode is on receives the system
key, encrypted only with the eQ-3 default key, which is public. Someone
within radio range who records the exchange, or whose own transmitter
announces itself first, gets your key, and with it can forge messages of
all your AES devices. So:

* pair AES devices only when nobody suspicious is within radio range, and
  keep pairing mode short (press the device's config button right away);
* check every result: hmbridge logs each hand-out (`pairing: KEY HAND-OUT:
  … serial …, RSSI … dBm`) and appends `system key handed out to serial …
  at RSSI … dBm` to "Last pairing result". If the serial is not the one on
  your device's label, or you did not pair anything, assume the key is
  known: change it (as in OpenCCU) and re-key your devices;
* `"pairing": {"aes": "off"}` pairs without giving out the key (devices then
  stay on the default key, without AES).

Messages you may see as the last result:

| result | meaning |
|---|---|
| `already known: …` | The device is in `config.json` or was paired before (e.g. taken over from OpenCCU, which uses the same address and key). hmbridge only wrote the central address again. |
| `… has model ID XXXX, which hmbridge does not support` | Not a supported model. |
| `… is paired to another central (…)` | The device belongs to another CCU. Factory-reset it (see its manual) or delete it in that CCU first. |
| `pairing … failed: … NACK` | The device refused the configuration; factory-reset it and try again. |
| `… AES key exchange failed …` | The device was paired but keeps its previous key, without AES. hmbridge tried the eQ-3 default key and then, for a device that still has a key from an earlier pairing, the system key and the previous key as its old key. Unpair it, factory-reset it and pair it again. |
| `… part 2 was not confirmed …` | The device did not acknowledge the second half of the key; it may use the system key or still the default key. Stored without AES and with key index 0; if commands to it fail, unpair, factory-reset and pair it again. |
| `… paired to this central address already, so it is assumed to use the system key …` | The device was paired to your central before (OpenCCU, or hmbridge before an unpairing it never confirmed) and is not in `config.json`: it was not given the key again. If its messages fail AES authentication, factory-reset and pair it again. |
| `ignoring DEVICE_INFO from 000000 / ffffff / <central>` | A spoofed announcement (no real device has these addresses); nothing was sent. |
| `… has the address of …` | Another model claims the address of a configured or paired device; nothing was sent. |

Paired devices are stored in `devices.json` in `state_dir` (default
`/var/lib/hmbridge/devices.json`, see section 4, *Persistence*), which hmbridge
reads at startup together with `config.json`. It is written only when a device is paired, unpaired or renamed.
If a device is in both files, `config.json` wins (logged at startup), and the
entry disappears from the devices file the next time it is written. To manage a
paired device in `config.json` instead (e.g. to put it into a heating group),
copy name, model, address, serial, `aes_channels` and `key_index` from the
devices file to `config.json`. On OpenCCU's Pi (section 3a), set
`state_dir` to a writable path such as `/usr/local/hmbridge/state`.
`"state_dir": ""` disables pairing. A paired device that hmbridge cannot use
(e.g. its `key_index` is no longer configured) is reported in the log and in
`GET /api/v1/bridge` (`warnings`) and kept in the file.

`pairing.aes`: `"system_key"` (the default when `security` is configured) or
`"off"` (devices stay on the eQ-3 default key without AES).

**Removing a device.** Devices paired with hmbridge have a button **Unpair** in
Home Assistant (in the diagnostic section, disabled by default so that it is
not pressed by accident: enable it first). It removes the device from hmbridge
and Home Assistant at once and tells the device to forget its central:
switches, dimmers and the LED display immediately, thermostats with a wake-up
burst. Window contacts, buttons, remotes and weather sensors only listen in
config mode: press their config button within the pairing duration after
Unpair. Devices from `config.json` cannot be unpaired from Home Assistant;
remove them from `config.json`. To pair a contact, button, remote
or weather sensor again while hmbridge still waits for it to confirm the
unpairing, start pairing mode and press its config button **twice**: the
first press completes the unpairing, the second one pairs it again.

**Before relying on it: check with a capture.** hmbridge's pairing follows
the protocol as documented by FHEM and Homegear, but has not been compared with
OpenCCU yet. Recording OpenCCU pairing one device lets you (and the hmbridge
developers) check every step:

1. Run `hmsniff` on OpenCCU (docs/SNIFFING.md) and pair one device in the
   WebUI (Teach-in devices → HM devices → press the config button).
2. Look for the lines `DEVICE_INFO …`, `CONFIG_START ch 0 … list 0`,
   `CONFIG_WRITE_INDEX ch 0 list 0 02=01 0A=… (central …)`, `CONFIG_END`,
   `AES_KEY_EXCHANGE … new key index N, part 1 of 2` and, for contacts,
   `CONFIG_WRITE_INDEX ch 1 list 1 08=01 (AES_ACTIVE=1)`. hmsniff never shows
   the key bytes of the key exchange, also not with `-no-redact`.
3. Do the same with `hmbridge -debug` pairing a similar device, and compare.
   Please report differences.

## 6. Device settings

The settings you would otherwise change in OpenCCU's WebUI ("Settings →
Devices → Settings" of a device: its MASTER parameters) appear in Home
Assistant on the device page under **Configuration**: numbers, selects,
switches and, for the thermostats, one text entity per weekday with the
week profile. Settings that belong to direct links between devices (e.g. a
dimmer's ramp times, which are link parameters) are not covered.

| model | settings (★ = enabled by default; the others are disabled, enable them in Home Assistant if you need them) |
|---|---|
| HM-CC-RT-DN | ★ comfort, eco, minimum and maximum temperature, ★ temperature offset (-3.5 … 3.5 K), ★ window open temperature and duration, ★ boost duration, ★ button lock, ★ week profile Monday … Sunday; window open detection, decalcification day and time, boost valve position, valve offset, maximum valve position, adaptive regulation, daylight saving time, boost after window open, display time or date, backlight on time, low battery limit |
| HM-TC-IT-WM-W-EU | ★ comfort, eco, minimum and maximum temperature, ★ temperature offset, ★ boost duration, ★ button lock, ★ display temperature (actual/set), ★ display humidity, ★ week profile (program 1); heating or cooling, daylight saving time, boost after window open, display time or date, low battery limit |
| HM-Sec-SCo | ★ event delay (0 … 127 s, above that whole minutes up to 7620 s), ★ cyclic status messages; sabotage message |
| HM-PB-2-WM55, HM-RC-8 | per button: long press time (0.3 … 1.8 s), double press time (0 … 1.5 s; 0 = off) |
| HM-LC-Sw1-Pl-DN-R1 | ★ power-on action (off / on: after a power failure the plug simulates a press of its own button) |
| HM-LC-Dim1L-Pl-3 | ★ power-on action; behaviour when the load reappears |
| HM-OU-LED16 | ★ display brightness (0 … 15) |

The weather sensors, the other contacts and window handles have no settings
worth exposing. Not exposed on purpose (the sources disagree on how they are
stored; see docs/DESIGN.md): global and mode button lock, local reset
disable, the LED display's energy option, the contacts' message for position
A/B. The wall thermostat's week programs 2 and 3 are not exposed.

**Reading.** hmbridge reads the settings from the device when it was just
paired, when you press **Refresh settings**, and when a change needs a value
it does not know yet — never periodically. The values are kept in
`settings.json` in `state_dir` (written only when they change), so after a
restart Home Assistant shows them at once. Until a device was read, its
settings are unknown: press "Refresh settings" once per device after
upgrading. If you change a setting in the device's own menu, press "Refresh
settings" (if the device reports the change itself, hmbridge takes it over).

**Changing.** A change is checked first: a value outside the device's range
or between its steps (e.g. 21.3 °C, a temperature offset of 4 K) is rejected
without any radio traffic. Otherwise hmbridge writes only the registers that
change, reads the list back and compares. The **Settings** sensor (diagnostic)
shows the outcome: `ok`, `writing`, `reading`, `pending: …` or
`failed: <setting> (see attributes)`; its attributes list the status per
changed setting (`ok`, `pending`, `writing`, `failed: <reason>`) and when the
settings were read. "Last command" shows the outcome too. Changes to devices
with AES use the same authenticated path as commands: the device's
acknowledgement must be authenticated.

**Battery devices.**

* Thermostats get the change at once, woken up with a burst like the climate
  commands (each message costs about 0.36 s of the 1 % airtime; a full week
  profile day is about seven messages).
* Window contacts and push buttons only listen right after their config
  button was pressed. A change shows `pending` until you **press the device's
  config button** (or the device signals that it listens); then hmbridge sends
  everything that waits, at once, and reads the settings back. "Refresh
  settings" waits for the config button as well. Pending changes are lost when
  hmbridge restarts.

**Week profile.** Each weekday is a text entity, e.g. "Week profile Monday",
with up to 13 entries `HH:MM temperature`, separated by commas: each entry is
the time up to which the temperature applies, the last one must be 24:00.
Times are multiples of 5 minutes, temperatures 5 … 30 °C in 0.5 steps:

```
06:00 17.0, 08:30 21.0, 16:00 17.0, 22:00 21.0, 24:00 17.0
```

means 17 °C until 06:00, 21 °C until 08:30, 17 °C until 16:00, 21 °C until
22:00 and 17 °C for the rest of the day. Several days at once (e.g. from an
automation or script) go to `hmbridge/<address>/set/setting_week_profile` as
JSON:

```yaml
action: mqtt.publish
data:
  topic: hmbridge/38f59c/set/setting_week_profile
  payload: '{"monday": "06:00 17, 22:00 21, 24:00 17", "tuesday": "06:00 17, 22:00 21, 24:00 17"}'
```

Either all days are valid and written, or none. The wall thermostat's
entities edit week program 1, the one it uses unless you selected another on
the device.

**MQTT.** Values and status are on `hmbridge/<address>/settings` (retained
JSON: `current`, `status`, `detail`, `read_at`); a change goes to
`hmbridge/<address>/set/setting_<key>`, a refresh to
`hmbridge/<address>/set/settings_refresh`.

**Please check on your devices** (the register layouts come from eQ-3's
device descriptions and FHEM, but have not been tried on real devices with
hmbridge yet): read a thermostat and compare the values with the device's
menu or OpenCCU; change the temperature offset and the week profile of one
day and check them on the device; on a window contact change the event delay
and press its config button; on a switch plug set the power-on action and
unplug it briefly. With an RPI-RF-MOD, check that reading a thermostat
completes (its list 7 is about 26 radio frames). Please report differences.

## Module firmware

An RPI-RF-MOD runs eQ-3's "dual coprocessor" firmware (4.x, e.g. 4.4.22),
which OpenCCU installs so that the module can talk Homematic IP as well.
hmbridge supports it (experimental): it detects the module, starts its
application if needed and does the BidCoS work the firmware leaves to the
host (ACKs, AES, retransmissions) itself. No firmware downgrade is needed,
and OpenCCU updating the module firmware does not get in the way.

An HM-MOD-RPI-PCB with the classic firmware (1.x) is supported as well.
