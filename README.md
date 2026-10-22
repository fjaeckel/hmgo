# hmbridge

hmbridge replaces a HomeMatic CCU (OpenCCU/RaspberryMatic, CCU2/3) for
**classic HomeMatic (BidCoS-RF)** heating devices. It drives an
**HM-MOD-RPI-PCB** radio module on a Raspberry Pi and exposes the devices to
**Home Assistant** via MQTT discovery, with **Prometheus** metrics for
everything.

It is a fork of Michael Stapelberg's [hmgo](https://github.com/stapelberg/hmgo),
a minimal Go implementation of the BidCoS protocol, extended with AES,
a configuration file, Home Assistant integration, window contacts and
heating groups.

## Features

* **Supported devices:** HM-CC-RT-DN (radiator thermostat), HM-TC-IT-WM-W-EU
  (wall thermostat), HM-Sec-SCo / HM-Sec-SC-2 / HM-Sec-SC (window contacts),
  HM-Sec-RHS (window handle).
* **No re-pairing.** hmbridge uses your CCU's radio address and AES key, so the
  devices keep talking to "their" central. Direct links between devices (wall
  thermostat ↔ valves, window contact → valve) are left untouched.
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
* **Heating groups**, like the CCU's: one climate entity per group that
  controls all its thermostats (or only the wall thermostat, which forwards to
  its valves, with `via_wall_thermostat`), with temperature from the wall
  thermostat and window state from the contacts.
* **Prometheus metrics** at `/metrics`, covering:
  * Per-device state.
  * The radio module (frames, acks, retries, duty cycle, errors).
  * MQTT and commands.
* **Status page** at `/` listing all devices, plus radio addresses that talk
  to the central but are not configured (handy for finding device addresses).
* **Survives restarts:** the last known state is read back from the MQTT
  broker, with a small state file as fallback that is written rarely to
  spare the SD card.
* **Robust:** a slow or unreachable MQTT broker never stalls the radio side.
  Commands from Home Assistant are coalesced (dragging a slider sends one
  radio command, not twenty) and retried when a device does not answer.

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

## Quick start

```sh
# Build for the Pi (pick the architecture of your Pi's OS).
GOOS=linux GOARCH=arm64 go build -o hmbridge ./cmd/hmbridge          # 64-bit OS
GOOS=linux GOARCH=arm GOARM=7 go build -o hmbridge ./cmd/hmbridge    # 32-bit OS, Pi 2/3/4
GOOS=linux GOARCH=arm GOARM=6 go build -o hmbridge ./cmd/hmbridge    # Pi 1 / Zero

# Write a configuration (see internal/config/example.json) and check it.
./hmbridge -config config.json -check_config

# Run.
./hmbridge -config config.json
```

`make dist` builds all of these into `dist/`.

See [docs/DEPLOY.md](docs/DEPLOY.md) for collecting the settings from OpenCCU,
switching between OpenCCU and hmbridge, and a systemd setup.

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
| `hmbridge/group/<name>/…` | the same for heating groups |

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
| `hmbridge_device_*` | Per device: last seen, RSSI, battery voltage and estimate, low battery, window, temperatures, valve, humidity, sabotage, faults, messages by AES result. |
| `hmbridge_uartgw_*` | Radio module: frames, acks by status, retries, send errors, request latency, duty cycle, firmware info. |
| `hmbridge_mqtt_*` | MQTT: connection state, publishes, commands, Home Assistant restarts. |
| `hmbridge_commands_total`, `hmbridge_command_queue_length` | Commands from Home Assistant. |
| `hmheating_*`, `hmthermal_*`, `hmcontact_*` | Raw decoded device events, kept compatible with hmgo. |

Example alerting rules are in
[contrib/prometheus/hmbridge.rules.yml](contrib/prometheus/hmbridge.rules.yml).

## Limitations

* No pairing of new devices and no editing of week programs or device
  settings yet. Use the CCU for that, then switch back.
* Homematic IP is not supported (and the dual firmware is not either).
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

BSD-3-Clause, see [LICENSE](LICENSE). Based on hmgo by Michael Stapelberg.
Protocol knowledge from [FHEM](https://fhem.de/) (HMUARTLGW, CUL_HM) and
[Homegear](https://homegear.eu/).
