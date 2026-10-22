<!-- SPDX-License-Identifier: BSD-3-Clause -->

# Deploying hmbridge

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
   warns (log, status page, `-check_config`, metric
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

Keep the default `"serial_port": "/dev/serial0"` and `"reset": "gpio"` (the
module's reset line is GPIO 18).

The status page and `/metrics` listen on all interfaces (`:8013`) so that
Prometheus can scrape them. They contain no keys or passwords, but they are
not harmless: they show every device's name, model, address, serial
number, state and battery level (which windows are open, whether someone is
home), and the status page can **start pairing mode** (button "Pair new
device"). The button needs a random token embedded in the page and a
request from the page itself, so other web sites cannot press it in your
browser; anyone who can open the page on your network can. Set
`"http_listen": "127.0.0.1:8013"` to keep it local (and scrape through an
SSH tunnel or a local Prometheus agent), or restrict port 8013 in your
firewall.

Requests must name hmbridge in their `Host` header: an IP address,
`localhost`, the machine's host name (also `<name>.local`), or a name listed
in `"http_allowed_hosts": ["hmbridge.example.lan"]` (e.g. behind a reverse
proxy; `"*"` accepts any name). Other names get "421 Misdirected Request".
This stops DNS rebinding, where a web site you visit re-points its own name
to hmbridge's address to read the status page from your browser.

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
entity and an "Unpair" button (disabled by default). hmbridge itself appears
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

To spare the SD card, a file is never rewritten with unchanged content.
Writes are atomic (a power cut leaves the old or the new version), files are
readable only by the hmbridge user (0600), and symbolic links are not
followed. If a file is damaged, hmbridge renames it to
`<name>.corrupt-<time>` (keeping the newest three), logs it, and starts
without it; for `devices.json` the status page shows a warning — repair the
copy and put it back while hmbridge is stopped. A `devices.json` written by a
newer hmbridge (after a downgrade) is left untouched and pairing is disabled
until you upgrade again or remove it. `"state_dir": ""` disables both files
(and pairing). Back up `devices.json` together with `config.json`.

*Upgrading:* the older options `state_file` and `devices_file` still work
(they name exact paths and are logged as deprecated). When you set a
`state_dir` other than `/var/lib/hmbridge`, hmbridge copies
`/var/lib/hmbridge/state.json` and `devices.json` into it on the first start
if they are not there yet, and leaves the old files in place. Files from
other locations (e.g. `/usr/local/hmbridge/devices.json` from an older
OpenCCU setup) are not copied: move them into `state_dir` yourself, while
hmbridge is stopped, then remove `devices_file` from the configuration.

The status page on port 8013 lists configured devices and every
radio address that talked to the central but is not configured — handy for
finding device addresses.

## 5. Adding devices

hmbridge can pair (teach in) new classic HomeMatic devices itself, so OpenCCU
is no longer needed for that. Supported are the models in the
[README](../README.md#supported-devices).

1. In Home Assistant, open the device **hmbridge** and press **Pair new
   device** (or press the button on the status page, port 8013). Pairing mode
   stays on for 60 seconds (`pairing.duration`); the sensors "Pairing mode",
   "Pairing time remaining" and "Last pairing result" show what happens.
   Anyone who can publish to the MQTT topic `hmbridge/bridge/set/pair` (any
   MQTT user with write access to `hmbridge/#`) or open the status page can
   start pairing mode too: give MQTT write access only to Home Assistant.
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
| `… AES key exchange failed …` | The device was paired but keeps its previous key (normally the eQ-3 default key), without AES. Unpair it, factory-reset it and pair it again. |
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
(e.g. its `key_index` is no longer configured) is reported in the log and on
the status page and kept in the file.

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

## Module firmware

An RPI-RF-MOD runs eQ-3's "dual coprocessor" firmware (4.x, e.g. 4.4.22),
which OpenCCU installs so that the module can talk Homematic IP as well.
hmbridge supports it (experimental): it detects the module, starts its
application if needed and does the BidCoS work the firmware leaves to the
host (ACKs, AES, retransmissions) itself. No firmware downgrade is needed,
and OpenCCU updating the module firmware does not get in the way.

An HM-MOD-RPI-PCB with the classic firmware (1.x) is supported as well.
