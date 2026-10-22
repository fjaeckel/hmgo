<!-- SPDX-License-Identifier: BSD-3-Clause -->

# hmbridge

hmbridge replaces a HomeMatic CCU (OpenCCU/RaspberryMatic, CCU2/3) for
**classic HomeMatic (BidCoS-RF)** devices: thermostats, window contacts,
switch and dimmer plugs, push buttons and remotes, the LED status display and
outdoor weather sensors. It drives an
**HM-MOD-RPI-PCB** radio module on a Raspberry Pi and exposes the devices to
**Home Assistant** via MQTT discovery, with **Prometheus** metrics for
everything.

It is a fork of Michael Stapelberg's [hmgo](https://github.com/stapelberg/hmgo),
a minimal Go implementation of the BidCoS protocol, extended with AES,
a configuration file, Home Assistant integration, window contacts and
heating groups.

## Features

* **Supported devices:** see the [table below](#supported-devices).
* **No re-pairing.** hmbridge uses your CCU's radio address and AES key, so the
  devices keep talking to "their" central. Direct links between devices (wall
  thermostat ↔ valves, window contact → valve) are left untouched.
* **Pairing new devices** without a CCU: press "Pair new device" in Home
  Assistant, then the device's config button.
  hmbridge configures it like a CCU (central address, AES key exchange to your
  system key, AES on contacts and buttons), keeps it in a small devices file
  and shows it in Home Assistant at once. Devices paired with hmbridge can be
  unpaired from Home Assistant. See
  [docs/DEPLOY.md](docs/DEPLOY.md#5-adding-devices).
* **AES** message authentication, handled by the radio module. Messages that
  fail authentication are dropped.
* **Home Assistant:** each thermostat appears as a climate entity (auto, heat,
  off, boost). Every value the devices report is exposed:
  * **Battery:** voltage, estimated level in %, low battery.
  * **Climate:** temperatures, humidity, valve opening, control mode,
    remaining boost time.
  * **Windows and faults:** window state, sabotage, fault codes.
  * **Radio:** signal strength, last seen, message count, AES result.
  * **Commands:** outcome of the last command (e.g. "failed: boost (no
    response from device)").
  * **Settings:** status of reading and writing the device settings.
* **Device settings** in Home Assistant (configuration entities): the
  thermostats' temperatures, offset, window and boost settings, button lock,
  decalcification, valve settings and week profile (one text entity per
  day), the window contact's event delay, the buttons' long and double press
  times, the plugs' power-on action, the LED display's brightness. Read when
  needed (after pairing, on "Refresh settings") and kept in `settings.json`;
  every value is range-checked before it is sent and read back after
  writing. Thermostats get changes at once with a wake-up burst; contacts and
  buttons show them as pending until their config button is pressed. See
  [docs/DEPLOY.md](docs/DEPLOY.md#6-device-settings).
* **Heating groups**, like the CCU's: one climate entity per group that
  controls all its thermostats (or only the wall thermostat, which forwards to
  its valves, with `via_wall_thermostat`), with temperature from the wall
  thermostat and window state from the contacts. The group's Home Assistant
  device also shows every member (valve positions, wall thermostat
  temperature and humidity, each window, availability) and, like OpenCCU's
  group parameters, **group settings** (comfort/eco temperature, week profile,
  window open temperature and duration, boost duration, minimum/maximum
  temperature) that are written to every thermostat of the group, with
  mixed values across the members flagged; see
  [docs/DEPLOY.md](docs/DEPLOY.md#heating-groups-in-home-assistant).
* **Prometheus metrics** at `/metrics`, covering:
  * Per-device state.
  * The radio module (frames, acks, retries, duty cycle, errors).
  * MQTT and commands.
* **Read-only JSON API** (`/api/v1/health`, `/bridge`, `/devices`,
  `/unknown`) for monitoring and scripts: health check (HTTP 503 when the
  radio is down), device state, and radio addresses that talk to the central
  but are not configured (handy for finding device addresses). There is no
  web UI; pairing, unpairing, renaming and settings are done in Home
  Assistant. See [docs/DEPLOY.md](docs/DEPLOY.md#http-api).
* **Survives restarts:** the last known state is read back from the MQTT
  broker, with a small state file as fallback that is written rarely to
  spare the SD card. Paired devices, that state and the device settings
  last read are kept as JSON files
  in one directory (`state_dir`, default `/var/lib/hmbridge`), written
  atomically; a damaged file is set aside, never blocking startup.
* **Robust:** a slow or unreachable MQTT broker never stalls the radio side.
  Commands from Home Assistant are coalesced (dragging a slider sends one
  radio command, not twenty) and retried when a device does not answer.

## Supported devices

| model | device | Home Assistant |
|---|---|---|
| HM-CC-RT-DN | radiator thermostat | climate, temperatures, valve, battery, fault |
| HM-TC-IT-WM-W-EU | wall thermostat | climate, temperature, humidity, battery |
| HM-Sec-SCo, HM-Sec-SC-2, HM-Sec-SC | window contact | window, sabotage, low battery |
| HM-Sec-RHS | window handle | window (closed/tilted/open), sabotage, low battery |
| HM-LC-Sw1-Pl-DN-R1 | switch plug | switch |
| HM-LC-Dim1L-Pl-3 | dimmer plug | light with brightness and transition (channel 1) |
| HM-PB-2-WM55, HM-RC-8 | 2-button wall switch, 8-button remote | an event entity per button (press_short, press_long, press_long_release) and device triggers for automations, low battery |
| HM-OU-LED16 | 16-LED status display | a select per LED (off, red, green, orange) |
| HM-WDS10-TH-O, HM-WDS30-T-O | outdoor sensor | temperature (and humidity), low battery |

Every device also gets diagnostic sensors (last seen, signal strength, AES
result, outcome of the last command). [docs/DEPLOY.md](docs/DEPLOY.md#4-home-assistant)
lists the entities per model. Switches, dimmers and the LED display are asked
for their status after startup and every hour (`status_poll_interval`);
remotes and weather sensors only send. On buttons with AES, the repeats sent
while a button is held are not authenticated, so `press_long` only arrives
together with `press_long_release` when the button is let go.

## Requirements

* A Raspberry Pi with one of eQ-3's radio boards (detected automatically,
  or set `"radio"` in the configuration):
  * **HM-MOD-RPI-PCB** with the classic firmware 1.x. The module handles
    acknowledgements and AES itself.
  * **RPI-RF-MOD** (eQ-3 "dual" firmware, as used by OpenCCU for Homematic IP
    and classic Homematic). **Experimental:** hmbridge acknowledges device
    messages and does AES in software, and parts of the module protocol are
    still being verified against recordings of OpenCCU's traffic
    ([docs/SNIFFING.md](docs/SNIFFING.md)).
* An MQTT broker reachable from the Pi (e.g. the Mosquitto add-on of Home
  Assistant).
* Raspberry Pi OS (a fresh installation needs no CCU: `hmbridge init`
  creates the configuration and a new system key), or an existing OpenCCU
  installation to take over.

## Quick start

**New system without a CCU** (Raspberry Pi OS Lite, see
[docs/DEPLOY.md](docs/DEPLOY.md#fresh-installation-no-openccu)):

```sh
# /boot/firmware/config.txt: enable_uart=1 and dtoverlay=disable-bt;
# remove console=serial0,115200 from cmdline.txt; reboot.
sudo useradd --system --no-create-home --shell /usr/sbin/nologin --groups dialout,gpio hmbridge

# Binary from a release (verify it, see DEPLOY.md).
gh release download nightly -R fjaeckel/hmgo -p 'hmbridge-*-linux-arm64' -p 'SHA256SUMS-*'
sha256sum --ignore-missing -c SHA256SUMS-* && gh attestation verify hmbridge-*-linux-arm64 -R fjaeckel/hmgo
sudo install -m 0755 hmbridge-*-linux-arm64 /usr/local/bin/hmbridge

sudo hmbridge probe     # resets and identifies the radio module
sudo hmbridge init      # writes /etc/hmbridge/config.json and a new AES system key
sudo -u hmbridge hmbridge -check_config

# contrib/systemd/hmbridge.service → /etc/systemd/system/, then:
sudo systemctl enable --now hmbridge
```

Then pair the devices one by one from Home Assistant (button "Pair new
device" on the device hmbridge).

**Taking over an OpenCCU installation** (same radio address and key, no
re-pairing; see [docs/DEPLOY.md](docs/DEPLOY.md#migrating-from-openccu)):

```sh
# Build for the Pi (pick the architecture of your Pi's OS), or take the
# binaries from a release.
GOOS=linux GOARCH=arm64 go build -o hmbridge ./cmd/hmbridge          # 64-bit OS
GOOS=linux GOARCH=arm GOARM=7 go build -o hmbridge ./cmd/hmbridge    # 32-bit OS, Pi 2/3/4
GOOS=linux GOARCH=arm GOARM=6 go build -o hmbridge ./cmd/hmbridge    # Pi 1 / Zero

# Write a configuration and check it: either run hmgenconfig on your
# OpenCCU (while it is running) and fill in the passphrase and MQTT
# settings, or write it by hand (see internal/config/example.json).
# -check_config fails until every CHANGE-ME placeholder is filled in.
./hmgenconfig > config.json
./hmbridge -config config.json -check_config

# Run.
./hmbridge -config config.json
```

`make dist` builds all of these into `dist/`.

See [docs/DEPLOY.md](docs/DEPLOY.md) for both paths in detail, switching
between OpenCCU and hmbridge, and the systemd setup.

## Configuration

A minimal configuration:

```json
{
  "central_address": "1A2B3C",
  "security": { "passphrase": "your CCU system security key", "index": 1 },
  "mqtt": { "broker": "tcp://homeassistant.local:1883", "username": "hmbridge", "password": "…" },
  "devices": [
    { "name": "Bad", "model": "HM-CC-RT-DN", "address": "38F59C", "aes": true },
    { "name": "Bad Fenster", "model": "HM-Sec-SCo", "address": "4C5D6E" }
  ],
  "groups": [
    { "name": "Bad", "members": ["Bad", "Bad Fenster"] }
  ]
}
```

[internal/config/example.json](internal/config/example.json) documents every
option. Omit `security` if you never set a system security key on your CCU.
On a new installation, `hmbridge init` writes the configuration and keeps a
newly generated key in a separate file (`"security": {"key_file":
"/var/lib/hmbridge/system.key", "index": 1}`, mode 0600) instead of
`passphrase`/`key`.

`cmd/hmgenconfig` generates the whole configuration (devices, names, AES
settings, heating groups) from a running OpenCCU; it reads nothing secret and
changes nothing. See "Generating the configuration" in
[docs/DEPLOY.md](docs/DEPLOY.md#generating-the-configuration).

## Home Assistant

hmbridge publishes MQTT discovery messages, so devices appear automatically
once the MQTT integration is set up. Topics, with base topic `hmbridge`:

| topic | content |
|---|---|
| `hmbridge/bridge/availability` | `online` / `offline` |
| `hmbridge/<address>/state` | JSON state of a device |
| `hmbridge/<address>/set/temperature` | e.g. `21.5` |
| `hmbridge/<address>/set/mode` | `auto`, `heat`, `off` |
| `hmbridge/<address>/set/preset` | `boost`, `none` |
| `hmbridge/<address>/set/switch` | `ON`, `OFF` (switch and dimmer plugs) |
| `hmbridge/<address>/set/light` | `{"state": "on", "brightness": 128, "transition": 2}` (dimmer) |
| `hmbridge/<address>/set/led_<n>` | `off`, `red`, `green`, `orange` (LED display, n = 1 … 16) |
| `hmbridge/<address>/event/button_<n>` | `{"event_type": "press_short", "button": 1}`, not retained (remotes) |
| `hmbridge/bridge/set/pair` | `PRESS` (or seconds, 10 … 600): start pairing mode |
| `hmbridge/bridge/pairing` | JSON pairing state: `status`, `active`, `remaining`, `last_result` |
| `hmbridge/<address>/set/unpair`, `…/set/name` | unpair / rename a device paired with hmbridge |
| `hmbridge/<address>/settings` | JSON device settings: `current` values, `status`, `detail` per setting, `read_at` |
| `hmbridge/<address>/set/setting_<key>` | change a setting, e.g. `setting_temperature_offset` `1.5`, `setting_week_profile_monday` `06:00 17, 22:00 21, 24:00 17` |
| `hmbridge/<address>/set/settings_refresh` | read the device's settings again |
| `hmbridge/group/<name>/…` | the same for heating groups |
| `hmbridge/group/<name>/members` | JSON member list of a heating group (attributes of its climate entity) |
| `hmbridge/group/<name>/settings` | JSON group settings: values the members agree on, per-member values and consistency, status of the last group change |
| `hmbridge/group/<name>/set/setting_<key>` | change a group setting on every thermostat of the group (`…/set/settings_refresh` reads them all) |

HomeMatic modes map to Home Assistant like this:

| HomeMatic | Home Assistant |
|---|---|
| auto | `auto` |
| manual | `heat` |
| manual at 4.5 °C | `off` |
| boost | preset `boost` |

The battery percentage is an estimate from the voltage, linear from 2.2 V
(0 %) to 3.0 V (100 %). The voltage itself is exposed too.

## Metrics

Everything is under `/metrics` on the status port (default `:8013`):

| prefix | covers |
|---|---|
| `hmbridge_device_*` | Per device: last seen, RSSI, battery voltage and estimate, low battery, window, temperatures, valve, humidity, sabotage, faults, messages by AES result, switch/dimmer level (`level_percent`), LED colors (`led_color`), button presses (`button_presses_total` by button and type), status requests. |
| `hmbridge_uartgw_*` | Radio module: frames, acks by status, retries, send errors, request latency, duty cycle, firmware info. |
| `hmbridge_mqtt_*` | MQTT: connection state, publishes, commands, Home Assistant restarts. |
| `hmbridge_commands_total`, `hmbridge_command_queue_length` | Commands from Home Assistant. |
| `hmbridge_pairing_*` | Pairing: attempts by result, unpairings, pairing mode active, number of paired devices. |
| `hmbridge_settings_operations_total` | Device settings: reads and writes by result (ok, failed, pending, rejected). |
| `hmheating_*`, `hmthermal_*`, `hmcontact_*` | Raw decoded device events, kept compatible with hmgo. |

Example alerting rules are in
[contrib/prometheus/hmbridge.rules.yml](contrib/prometheus/hmbridge.rules.yml).

## Limitations

* No editing of direct links (peerings) and their parameters (e.g. a
  dimmer's ramp times). Use the CCU for that, then switch back. Pairing with
  hmbridge sets up the device for hmbridge only, without direct links
  between devices. The device settings follow eQ-3's device descriptions and
  FHEM but have not been tried on every model yet (see
  [docs/DEPLOY.md](docs/DEPLOY.md#6-device-settings)); a few settings whose
  sources disagree are not exposed.
* Pairing follows FHEM's and Homegear's description of the CCU's pairing; the
  AES key exchange is verified against FHEM's code but not yet against a real
  device or an OpenCCU capture (see [docs/DESIGN.md](docs/DESIGN.md#pairing)).
* Of the dimmer plug only the physical channel 1 is exposed (2 and 3 are
  virtual channels).
* Homematic IP is not supported yet. On the RPI-RF-MOD, hmbridge only lists
  the Homematic IP devices it hears; see [docs/HMIP.md](docs/HMIP.md).
* Commands to battery devices are sent as wake-up bursts, which use the radio's
  duty cycle (1 % airtime). Normal use is far below the limit; the duty cycle
  is exported as a metric.

## Recording OpenCCU's radio traffic

`cmd/hmsniff` passively records and decodes the traffic between OpenCCU and
its radio module while OpenCCU keeps running; see
[docs/SNIFFING.md](docs/SNIFFING.md).

## Development

```sh
go test -race ./...
```

The tests need no hardware. `internal/uartgw/fakemod` emulates the radio
module, and the end-to-end test in `cmd/hmbridge` additionally runs against a
local `mosquitto` if it is installed. [docs/DESIGN.md](docs/DESIGN.md)
describes the architecture and the radio protocol.

## License

BSD-3-Clause, see [LICENSE](LICENSE). hmbridge is a fork of Michael
Stapelberg's [hmgo](https://github.com/stapelberg/hmgo) (Copyright (c) 2017
Michael Stapelberg); files that come from hmgo carry his copyright line in
their header. Michael Stapelberg does not endorse this fork.

`hmbridge -license`, `hmsniff -license`, `hmgenconfig -license` and
`hmkeycheck -license` print the license. Protocol
knowledge comes from [FHEM](https://fhem.de/) (HMUARTLGW, CUL_HM),
[Homegear](https://homegear.eu/) and [RFNETHM](https://github.com/tostmann/RFNETHM);
no code from these GPL projects is included. The AES test vectors in
`internal/bidcos/aes/testdata` were produced by running FHEM's code; they are
data, not code.
