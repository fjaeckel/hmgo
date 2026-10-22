<!-- SPDX-License-Identifier: BSD-3-Clause -->

# hmbridge design

hmbridge turns a Raspberry Pi with an HM-MOD-RPI-PCB radio module into a
small, single-purpose replacement for a HomeMatic CCU: it speaks classic
HomeMatic (BidCoS-RF) to thermostats and exposes them to Home Assistant via
MQTT discovery. It is a fork of github.com/stapelberg/hmgo (module path
github.com/fjaeckel/hmgo).

## Goals (prototype)

* Supported devices: HM-CC-RT-DN (radiator valve drive, “heating” package)
  and HM-TC-IT-WM-W-EU (wall thermostat, “thermal” package); later window
  contacts (“contact”), switch and dimmer plugs (“power”), remotes
  (“remote”), the LED display (“led16”) and weather sensors (“weather”), see
  “Other device families” below.
* Take over existing devices without re-pairing: hmbridge uses the
  CCU's BidCoS address (“central address”) and the CCU's AES key, so the
  devices keep talking to “their” central.
* AES message authentication, handled by the radio module itself (we
  load keys and mark AES channels per peer; no crypto in Go).
* Home Assistant: one `climate` entity per device plus sensors, via MQTT
  discovery. Setting target temperature and mode from HA.
* Configuration in one JSON file; no hard-coded devices.
* Testable without hardware: an emulated radio module (fakemod) and an
  end-to-end test against a local mosquitto.

Non-goals for the prototype: editing week programs, Homematic IP,
HM-MOD-RPI-PCB firmware updates. (Pairing new devices, the
dual-coprocessor firmware and device settings including the week profile
were added later, see “Pairing”, “RPI-RF-MOD” and “Device settings”.)

## Architecture

```
            serial (115200 8N1)
HM-MOD-RPI-PCB ──────────────── uartgw.Conn ── bidcos.Sender ── hm/heating, hm/thermal
                                     │                                   │
                                Recv() frames                    command encoders
                                     ▼                                   │
                               bridge.Bridge  ◀──────────────────────────┘
                                     │  ▲
                        state msgs   ▼  │  commands
                                  ha.Client (MQTT, paho)
                                     │
                               Home Assistant
```

Package ownership (one owner per package while building in parallel):

| package | purpose |
|---|---|
| `internal/hmkey` | AES key type, eQ-3 default key, passphrase → key (MD5) |
| `internal/bidcos` | BidCoS packet encode/decode, `Auth()`, `RSSI()`, `Transport`, `Sender` |
| `internal/uartgw` | HM-MOD-RPI-PCB frame protocol, concurrent `Conn` |
| `internal/uartgw/fakemod` | emulated HM-MOD-RPI-PCB for tests |
| `internal/config` | JSON config, defaults, validation |
| `internal/hm/...` | device models: event decoders (existing) + command encoders |
| `internal/hm/devices` | model name → device model, shared by hmbridge and hmsniff |
| `internal/hm/pairing` | pairing protocol: model ID table, DEVICE_INFO, CONFIG_* encoders |
| `internal/hm/params` | device settings: register layout, encoding and validation per model, register read answers, week profile |
| `internal/store` | persistence: the state directory with `state.json`, `devices.json` and `settings.json` |
| `internal/ha` | HA discovery payloads, state/command topics, MQTT client |
| `internal/bridge` | glue: registry, event loop, command handling, read-only HTTP API |
| `cmd/hmbridge` | main: flags, serial setup, GPIO reset, signal handling |

## Radio module protocol (HM-MOD-RPI-PCB, classic firmware 1.x)

Reference: FHEM `00_HMUARTLGW.pm`, hmgo `internal/uartgw`.

Frame: `0xFD | len(2, BE) | dst | cnt | cmd | payload | crc16(2, BE)`.
`len` counts dst+cnt+cmd+payload. CRC: poly 0x8005, init 0xD77F, no
reflection, computed over the whole unescaped frame from 0xFD on.
Escaping after the first byte: 0xFC/0xFD → 0xFC, byte&0x7F.

Destinations: 0 = OS (bootloader), 1 = App, 254 = Dual, 255 = DualErr.
After a reset the module announces `Co_CPU_BL` (cmd 0x00 on dst 0);
`OSChangeApp` (0x03) switches to the app, which announces `Co_CPU_App`.
The dual-coprocessor firmware (RPI-RF-MOD, also used for Homematic IP)
answers on dst 254/255 or announces `DualCoPro_App`. The classic driver
(`uartgw.Open`) rejects it with `uartgw.ErrDualCoProFirmware`;
`uartgw.OpenModule` detects it and uses the RPI-RF-MOD driver
(`uartgw.OpenDual`, see docs/SNIFFING.md for the protocol as captured).

Commands we use: OS 0x02 get firmware, 0x06 normal mode (leave update mode), 0x0A CSMA/CA (off by default, like FHEM/Homegear), 0x0B get
serial, 0x0E set time (uint32 BE unix time + int8 UTC offset in 30 min);
App 0x00 set HMID, 0x02 send, 0x03 set current key, 0x0F set previous
key, 0x0B set temp key (key payload: 16 key bytes + 1 index byte; 17
zero bytes clears a slot), 0x06 add peer (`addr(3) keyIndex wakeup 00`), 0x07 remove peer
(`addr(3)`, after unpairing),
0x09 peer enable AES (`addr(3)` + list of channel numbers), 0x0A peer
disable AES (same format).

The module answers every request with an ack frame, cmd 0x04 on the
request's destination, carrying the same counter. Ack status (first
payload byte): 0x00 NACK, 0x01 ACK, 0x02 ACK+info, 0x03 ACK with device
response, 0x04 unknown error (typically: device did not answer), 0x05 no
credits (duty cycle exhausted), 0x06 CSMA/CA failure (retry), 0x07
multipart data, 0x08 in progress (retry), 0x0C ACK with AES-OK response,
0x0D ACK with AES-failed response. For 0x03/0x0C/0x0D the rest of the
payload is `info rssi msg…`, i.e. the device's response frame.

Received radio frames: cmd 0x05 on dst 1, payload `status info rssi
msg…`. Status 0x01/0x11 normal, 0x02/0x12 AES OK, 0x03 AES failed.

## RPI-RF-MOD (eQ-3 dual firmware)

`uartgw.OpenModule` detects the module: a startup message or the reply to
IDENTIFY (dst COMMON, payload 01) names the application, `Co_CPU_*` for the
HM-MOD-RPI-PCB and `HMIP_TRX_Bl` / `DualCoPro_App` for the RPI-RF-MOD.
`uartgw.DualConn` implements the same `uartgw.Module` interface as the
classic `uartgw.Conn`, so the bridge doesn't care which one is attached.

Known from the open-source RFNETHM project (protocol reference only):
same frame format; dst 0x03 LLMAC carries classic BidCoS: LLMAC_SEND
`06 80 00 80 <msg>`, acknowledged by LLMAC_ACK `01 <status>` with the same
frame counter; received messages arrive as LLMAC_RECV
`05 <ts:2> <info> <rssi> <msg>`; CHANGE_APP from the bootloader is `03` on
dst COMMON.

Unlike the HM-MOD-RPI-PCB, the module apparently neither acknowledges device
messages nor does AES, so `DualConn` does both in software
(`internal/bidcos/aes`, implemented independently from the protocol as
documented by FHEM/Homegear and verified against FHEM-generated test vectors): it ACKs BiDi messages from registered peers, challenges
messages from AES peers and verifies the answers, answers devices' AES
challenges to our commands, correlates device responses with sent commands,
and retransmits unanswered commands. Whatever OpenCCU's multimacd sends the
module at startup (address, peers) is still unknown; `Config.DualInit`
takes those steps once a capture (`cmd/hmsniff`) shows them.

## Interfaces

### uartgw

```go
type Config struct {
    HMID     [3]byte      // the central address to impersonate
    Current  *hmkey.Key   // nil: clear slot (no AES)
    Previous *hmkey.Key   // nil: clear slot
    Now      func() time.Time // nil: time.Now
}
var ErrDualCoProFirmware error

// Open performs the init handshake on a freshly reset module and starts
// the reader goroutine. rw is typically the serial port.
func Open(rw io.ReadWriter, cfg Config) (*Conn, error)

func (c *Conn) FirmwareVersion() string
func (c *Conn) SerialNumber() string
func (c *Conn) AddPeer(ctx context.Context, p Peer) error
func (c *Conn) SetTime(ctx context.Context, t time.Time) error
// Send implements bidcos.Transport. It retries on 0x06/0x08 acks, maps
// 0x00/0x04/0x05/0x0D to errors (ErrNoResponse, ErrNoCredits,
// ErrAESFailed, ...) and normalises the response frame's status byte to
// receive numbering (0x03→0x01, 0x0C→0x02, 0x0D→0x03).
func (c *Conn) Send(ctx context.Context, encoded []byte) ([]byte, error)
// Recv delivers received radio frames (AppRecv payloads, decodable with
// bidcos.Decode). Closed when the connection fails; see Err.
func (c *Conn) Recv() <-chan []byte
func (c *Conn) Err() error
func (c *Conn) Close() error

type Peer struct {
    Addr        [3]byte
    KeyIndex    byte   // key index the device uses (0 = eQ-3 default)
    Channels    int    // number of channels (AES is disabled on all others)
    AESChannels []byte // channels on which the module enforces AES
    Wakeup      bool
}
```

Requests are serialized (one outstanding request); acks are matched by
counter and late/stale acks are dropped. An unexpected `Co_CPU_BL` /
`Co_CPU_App` announcement while running means the module reset: the
connection fails with an error so the process can exit and be restarted.

### hm devices

Existing decoders stay. New command encoders on `heating.Thermostat`
(control channel 0x04) and `thermal.ThermalControl` (control channel
0x02), sent as BidCoS cmd 0x11 with flags `BiDi|RepeatEnable|Burst`:

| action | payload |
|---|---|
| set target temperature | `86 <chn> <temp*2>` |
| auto mode | `80 <chn>` |
| manual mode | `81 <chn> <temp*2>` |
| boost | `83 <chn>` |

Temperatures 4.5 (= off) … 30.5 (= on) in 0.5 steps. Methods take a
`context.Context` and return the decoded device response (ACK / status).

### ha

Topics (base topic `hmbridge` by default, discovery prefix
`homeassistant`):

* `hmbridge/bridge/availability` — `online`/`offline` (LWT).
* `hmbridge/<addr>/state` — retained JSON state per device.
* `hmbridge/<addr>/set/temperature` — payload `21.5`.
* `hmbridge/<addr>/set/mode` — `auto` | `heat` | `off`.
* `hmbridge/<addr>/set/preset` — `boost` | `none`.

HA modes: HM auto → `auto`, HM manual → `heat`, manual at 4.5 °C → `off`.
Boost is a preset. Discovery: one HA device per BidCoS device with a
`climate` entity and sensors (battery voltage, valve position for
RT-DN, humidity for TC, fault for RT-DN, window open for TC, RSSI).

### config

```json
{
  "serial_port": "/dev/serial0",
  "reset": "gpio",
  "central_address": "1A2B3C",
  "security": { "passphrase": "…", "index": 1 },
  "mqtt": { "broker": "tcp://homeassistant.local:1883", "username": "", "password": "" },
  "http_listen": ":8013",
  "devices": [
    { "name": "Wohnzimmer", "model": "HM-CC-RT-DN", "address": "38F59C", "aes": false }
  ]
}
```

`security` may give `key` (32 hex chars) instead of `passphrase`; omitted
means the eQ-3 default key (index 0). `previous` holds an optional older
key in the same format.

### HTTP API

`http_listen` (default `:8013`) serves `/metrics` and a read-only JSON API
(`bridge.API`, `internal/bridge/api.go`; user documentation in DEPLOY.md,
“HTTP API”): `/api/v1/health` (200 ok/degraded, 503 down: radio module
failed), `/api/v1/bridge`, `/api/v1/devices[/{address}]`,
`/api/v1/groups`, `/api/v1/unknown`, and `/` as an index. There is no web
UI and no state-changing endpoint: every action (pairing, unpairing,
renaming, settings) is an MQTT command, so the HTTP side needs no CSRF
token, origin checks or frame protection. Hardening that remains:

* `bridge.HostAllowlist` on every request (DNS rebinding: a foreign name in
  `Host` gets 421; IP literals, localhost, the host name and
  `http_allowed_hosts` are accepted);
* `bridge.ReadOnly`: anything but GET/HEAD gets 405 with `Allow: GET, HEAD`;
* server timeouts, 16 KiB header limit, at most 64 connections
  (`cmd/hmbridge/httpserver.go`);
* handlers copy what they need under `b.mu` and encode outside it;
  responses are bounded (4 MiB; the data is bounded anyway);
* no key material in any response: devices show AES channels and key
  index only (tested by scanning every response for the keys in hex,
  base64 and raw form).

`/api/state` (the JSON of the former HTML status page) is kept as a
deprecated alias for one release (`Deprecation: true` header).

## Bridge concurrency

`bridge.Run` owns all device state on one event loop: it decodes
received frames, applies results of executed commands and translates
Home Assistant commands into radio jobs. Two helper goroutines keep the
loop responsive:

* the **worker** executes radio jobs one at a time (a burst command can
  take seconds). Jobs are coalesced per device and kind (temperature vs.
  mode), so only the latest queued value is sent; unanswered commands are
  retried twice after 20 s unless superseded by a newer command.
* the **publisher** sends MQTT messages from an outbox that keeps only
  the latest message per topic. State snapshots are enqueued under the
  bridge mutex, so an older snapshot (e.g. from a reconnect) can never
  overwrite a newer one, and a slow broker never blocks the radio side.

* **last command wins:** a newer command for a device moves to the back of
  the queue, and a retry is dropped if any newer command for that device
  was queued since (otherwise a retried “22 °C” could undo a later “off”).

Time requests (cmd 0x3F) from the devices are answered with Homegear's
format (UTC offset + seconds since 2000) as priority jobs, and the
module's clock is set every `time_sync_interval` — both only while the
kernel reports the system clock as NTP-synchronised (`require_synced_clock`).

Robustness:

* **availability:** each device has a retained `<base>/<addr>/availability`
  topic; entities are available only while the bridge and the device are
  online. A device is offline once silent for longer than its stale
  timeout (30m thermostats, 1h weather sensors, 6h HM-Sec-SCo, 50h other
  contacts, 3 status poll intervals for actuators, never for remotes).
* **persistence, SD-card friendly:** after a restart the state is read back
  from the broker's retained state messages (no writes on the Pi). A small
  state file is the fallback when the broker is unreachable at startup; a
  separate goroutine writes it only after significant changes (mode, target,
  window, battery, fault; not temperature/valve/RSSI reports), at most every
  `state_save_interval` (15m), and on shutdown. An empty state is never
  published. A restored boost that has expired is dropped. The file is
  `state.json` in the state directory (see **Persistence** below).
* **availability** is measured from the device's last message, also across
  restarts, so a device that died before a restart is unavailable at once;
  groups are available while any of their thermostats is.
* **AES:** for AES devices, frames not addressed to the central (which the
  module does not authenticate) never change the state.
* **module clock:** Open leaves the module's clock alone while the system
  clock is not NTP-synchronised (`uartgw.Config.SkipSetTime`); the bridge
  sets it once it is.
* **wedged module:** after 3 consecutive requests the module never
  acknowledged, the connection fails (`uartgw.ErrModuleUnresponsive`) and
  the process exits for systemd to restart it. Module firmware ≤ 1.0.6
  is refused (`uartgw.ErrFirmwareTooOld`).

## AES enforcement

Anyone in radio range can send frames with a device's address, and
neither radio module challenges every frame of an AES device: frames
without BiDi (the device does not wait for a challenge), ACKs (cmd 0x02)
and frames whose payload names a channel without AES pass with status
0x01 (unauthenticated). The bridge therefore decides per frame
(`internal/bridge/aes.go`); for a device with AES, an unauthenticated
frame changes nothing unless it is:

* DEVICE_INFO (0x00) or a time request (0x3F): never state; time
  responses are rate-limited (below);
* a KEY_EVENT/SENSOR_EVENT/INFO_LEVEL addressed to the central whose
  channel (0x40/0x41: byte 0, 0x10 06: byte 1) exists and has no AES
  (e.g. a remote with AES on some buttons only).

Everything else is dropped and counted in
`hmbridge_aes_rejected_total{reason}`: frames not addressed to the
central (broadcasts and direct links can never be authenticated; like
Homegear, `BidCoSPeer::packetReceived`), unsolicited ACK/ACK_STATUS
(answers to our commands are consumed by `Send`), channels the device
does not have, unauthenticated frames of AES channels (e.g. without
BiDi) and frames that do not name their channel (e.g. a thermostat's
INFO 0x10 0A). Real AES devices send their state to the central with
BiDi, so it is challenged (DualConn challenges every BiDi frame of an
AES peer whose channel is unknown; FHEM only challenges 0x40/0x41/0x10
04/06, Homegear every non-ACK frame). Consequences: an AES thermostat's
broadcasts (climate event 0x58, weather 0x70) were already ignored; with
an HM-MOD-RPI-PCB, whose per-channel challenge rule is not documented, a
frame of an AES device the module did not challenge is now ignored
instead of applied. Remote long-press repeats (no BiDi) on AES buttons
are dropped as before. Devices without AES are not affected. The
decoders also reject channels the device does not have (contacts:
channel 1 only; remotes: 1..buttons).

**Answers to our commands.** `hm.StandardDevice.CheckReplyAuth`: the
ACK/ACK_STATUS to a command for an AES channel (SET 0x11: byte 1, CONFIG
0x01: byte 0) must be authenticated: a real device challenges such a
command, so its final ACK is (HM-MOD-RPI-PCB 0x0C; DualConn checks the
auth bytes). A plain ACK fails the command
(`hm.ErrUnauthenticatedReply`, retried). Like Homegear (`validAesAck`
for 0x02 only) and FHEM, the INFO_LEVEL answer to a status request is
not required to be authenticated: a forger racing the device can still
fake one poll result (no action on the device; the next event or poll
corrects it). DualConn answers a device's challenge only with the
configured key index of that peer (also shortly after RemovePeer, for
unpairing), never with another key, e.g. the default key (index 0).

**Duty cycle.** DualConn sends at most 10 AES challenges (new and
re-sent ones) per peer and 10 s, and software ACKs from a per-peer
token bucket (10, refilled with 1/s; AES-verified final ACKs are not
limited); the bridge answers at most 2 time requests per device at
once, then one per minute. Refusals are counted in
`hmbridge_uartgw_dual_rate_limited_total{kind}` and
`hmbridge_rate_limited_total{kind="time_response"}`. The module's duty
cycle: `hmbridge_uartgw_duty_cycle_percent` (HM-MOD-RPI-PCB) and
`hmbridge_uartgw_dual_trx_status` (RPI-RF-MOD, meaning unconfirmed).
Log lines anyone can trigger (failed AES, undecodable frames, unknown
senders, dropped frames) are rate-limited per kind (`internal/ratelog`:
10 resp. 5 per minute, then one summary line).

## Other device families

Layouts from eQ-3's device descriptions (rftypes XML, as shipped with
Homegear) unless noted; payload byte 0 is the first byte after the
addresses. “Captured” means seen in the user's hmsniff recordings of
OpenCCU.

| model | package | frames used |
|---|---|---|
| HM-LC-Sw1-Pl-DN-R1 (1 ch) | `hm/power` | LEVEL_SET `11 02 ch level ramp(2) [on_time(2)]`; LEVEL_GET `01 ch 0E` → INFO_LEVEL; ACK_STATUS `02 01 ch level flags rssi`; INFO_LEVEL `10 06 ch level flags` |
| HM-LC-Dim1L-Pl-3 (ch 1 real, 2-3 virtual) | `hm/power` | as the switch; flags bits 4-5 direction (1 up, 2 down), 4-6 non-zero while working, bits 1-3 ERROR (3 = LOAD_FAILURE) |
| HM-PB-2-WM55 (2 ch), HM-RC-8 (8 ch) | `hm/remote` | KEY_EVENT `40 btn counter`: btn bits 0-5 channel, bit 6 long, bit 7 low battery |
| HM-OU-LED16 (16 ch) | `hm/led16` | SET_ONE_CHANNEL `11 80 ch color` (captured from OpenCCU, flags A0); GET `01 ch 0E`; ACK_STATUS/INFO_LEVEL `… ch color`; colors 0 off, 1 red, 2 green, 3 orange |
| HM-WDS10-TH-O, HM-WDS30-T-O (1 ch, no AES) | `hm/weather` | WEATHER_EVENT `70 t_hi t_lo [hum]`: 15-bit signed temperature ×0.1 °C, bit 15 low battery, humidity % (TH only); captured `006450` = 10.0 °C 80 %, `008264` = 13.0 °C (third byte unused on the T-O) |

Level 0x00…0xC8 = 0…100 % in 0.5 % steps; times (ramp, on time) are
eQ-3's 16-bit tiny float: mantissa bits 5-15, exponent bits 0-4, value =
mantissa·2^exponent·0.1 s. Actuator commands are BiDi without burst (the
devices are mains powered and always listening).

Not verified against the real devices yet (assumptions): the low battery
bit (bit 7) of KEY_EVENT and WEATHER_EVENT (FHEM reads it so; eQ-3 derives
LOWBAT “internally”); which flags the remotes' long-press repeats carry;
the dimmer's ACK_STATUS while ramping.

**Remotes.** eQ-3 defines PRESS_LONG_RELEASE as KEY_EVENT_LONG_BIDI: a long
KEY_EVENT with the BiDi flag; repeats while a button is held are sent
without BiDi, with the press counter unchanged. `remote.Tracker` turns the
frames into events per button: short → `press_short` (a repeated counter
within 10 s is a retransmission); the first long frame of a counter →
`press_long`; long+BiDi → `press_long_release` (preceded by a synthetic
`press_long` if none was seen); no repeat or release for 5 s, or a frame
with a new counter, → synthetic `press_long_release`. Events of AES
channels are only accepted when authenticated, so on AES buttons the
unauthenticated repeats are dropped and the long press arrives with the
release. Events are published to `<base>/<addr>/event/button_<n>`, not
retained, and the outbox never coalesces them (`outbox.putEvent`); they are
neither persisted nor part of the state. Home Assistant gets an `event`
entity per button and `device_automation` triggers (value template
`value_json.event_type`).

**Actuators.** Commands are jobs like the climate commands, coalesced and
superseded per output (`level:<addr>`, `led:<addr>:<n>`; the LEDs are
independent), retried twice when unanswered, and reported in
`last_command`. The device's direct answer (ACK_STATUS) is applied
immediately; a plain ACK or a ramping ACK_STATUS shows the commanded level
until the final INFO_LEVEL. A status request (LEVEL_GET) is answered with
INFO_LEVEL instead of an ACK; the radio layer returns it like any response.
Status requests go out once after startup (staggered by 2 s) and every
`status_poll_interval`; they are never retried, failures are only logged
with `-debug` (availability shows an unreachable device). Switch level,
dimmer level (and the last level > 0, used for “on” without brightness)
and LED colors are significant for the state file.

**RPI-RF-MOD.** The software ACK path already acknowledges every BiDi
message of a configured peer to the central, including KEY_EVENTs. AES
challenges now follow the peer's AES channels where the channel is
unambiguous (KEY_EVENT 0x40 and SENSOR_EVENT 0x41: low 6 bits of byte 0),
like the radio modules' per-channel AES table; other messages of AES peers
are challenged as before.

## Pairing

`internal/bridge/pairing.go` pairs and unpairs devices like a CCU; the
protocol pieces are in `internal/hm/pairing` and
`internal/bidcos/aes/keyexchange.go`. Sources: FHEM 10_CUL_HM.pm
(DEVICE_INFO handling with hmPairForSec, CUL_HM_pushConfig, assignHmKey),
HMConfig.pm (model IDs, register definitions), Homegear
HomeMaticCentral.cpp (handlePairingRequest, unpair, reset), BidCoSPeer.cpp
(key change queue), AesHandshake.cpp (generateKeyChangePacket),
Hm-Mod-Rpi-Pcb.cpp, and eQ-3's device descriptions as shipped with Homegear
(model IDs, AES defaults, receive modes). All GPL: read as references
only, no code copied.

**Flow.** Pairing mode (HA button `hmbridge/bridge/set/pair`, MQTT only;
default 60 s) is state of the event loop. A DEVICE_INFO
(cmd 0x00: `fw model(2) serial(10) class peer-channels…`) while it is on
becomes a priority radio job, so it runs while the device is still in
config mode. Priority jobs run before all others, FIFO among themselves;
a pair job drops a queued unpair job of the same address and vice
versa. Before anything is sent, the sender is checked: 000000, FFFFFF and
the central's own address are refused (`invalid_source`), as are other
models (or serials) at the address of a configured, paired or kept
devices-file entry. Rejections are rate-limited in the log (one line per
kind and minute):

| step | messages (flags 0xA0 BIDI\|RPTEN, each ACKed `02 00`) |
|---|---|
| register | `AddPeer` (key index 0, no AES; the current index for a device already paired to our address, see below) |
| central address | `01 00 05 000000 00 00` (CONFIG_START ch 0 list 0), `01 00 08 02 01 0A hh 0B mm 0C ll` (WRITE_INDEX; reg 02 = 0x81 for models with INTERNAL_KEYS_VISIBLE: switch, dimmer), `01 00 06` (CONFIG_END) |
| key exchange (pairing.aes system_key, AES-capable model, key index > 0) | `04` × 2, see below; the device answers each with an AES challenge with its old key (index 0), which the radio layer answers |
| AES_ACTIVE (channels with aes_default in eQ-3's descriptions: contacts ch 1, PB-2-WM55 ch 1-2, RC-8 ch 1-8) | per channel `01 ch 05 000000 00 01`, `01 ch 08 08 01`, `01 ch 06` |
| register | `AddPeer` with the new key index and AES channels |

No burst: a battery device in config mode listens. The result (device with
name "<model> <serial>", AES channels, key index, firmware, model ID) is
written to the devices file, the device joins the registry under `b.mu`,
its discovery, name and availability are published, and actuators get a
status request. Pairing mode then ends (one device at a time). A DEVICE_INFO
from a known address (config.json or devices file, same model) only gets
the central address sequence (“already known”: devices from OpenCCU share
our address and key); one addressed to another central, an unsupported model
ID, or a different model at a configured address are rejected. A DEVICE_INFO
addressed to our central from an address that is *not* configured comes
from a device paired to this central before (OpenCCU, or hmbridge before an
unpairing the device never confirmed): it is not factory-new, so it gets no
key exchange; it is assumed to use the current system key (stored key
index, AES_ACTIVE as for a new device), which the result says.

**Key hand-out (pairing.aes `system_key`, the default like OpenCCU).** The
key exchange gives the system key to whatever device announced itself,
encrypted only with the publicly known eQ-3 default key: an attacker in
radio range who answers in time, or records the exchange, learns it.
hmbridge therefore logs every hand-out (`KEY HAND-OUT`, with address, model,
serial and RSSI) and appends serial and RSSI to the last pairing result in
Home Assistant. A key exchange whose part 2 was not acknowledged (no
answer, not a NACK) leaves the device's key unknown; the result says so
(stored as index 0, AES off) instead of asserting the default key.

**Unpair** (HA button `set/unpair`, only devices from the devices file):
the device is removed from registry, devices file and HA (empty retained
discovery/state/name), then gets the central address sequence with 000000
and reg 02 = 00 (Homegear `unpair`); always listening devices at once,
wake-on-radio thermostats with burst (flags 0xB0), config-only devices
(contacts, buttons, remotes, weather sensors) when their next DEVICE_INFO
(same model) arrives within the pairing duration. The radio module peer
(`RemovePeer`) is removed only after the unpair messages were sent (also
when they failed), or when the wait for the config button ended: the
module needs the device's key index and AES channels to answer its AES
challenges. Clean-up after a failed or timed-out job uses a fresh 5 s
context. Homegear's factory reset (`11 04 00`) is not used.

**Devices file** (`devices.json`, see **Persistence**): written only on
pair/unpair/rename, synchronously but outside `b.mu` (a snapshot is taken
under the lock; a sequence number keeps an older snapshot from being written
after a newer one). Read in `bridge.New` after config.json's devices; an
address or name conflict is resolved in favour of config.json and logged.
Store devices may use key index 0 (`config.ResolveStoreDevice`); a record
that does not resolve (e.g. its key index is no longer configured) is
logged, reported by the API (`/api/v1/bridge` warnings) and written back unchanged, so fixing the
configuration brings it back. Renaming replaces the device entry (jobs of
the old entry are applied to the new one by address).

**Persistence** (`internal/store`). Everything hmbridge writes lives in
one directory, `state_dir` (default `/var/lib/hmbridge`, absolute, `""`
disables all files and pairing): `state.json` (device state fallback,
compact), `devices.json` (paired devices, indented) and `settings.json`
(device settings cache, compact, see “Device settings”). Further data
(e.g. HmIP keys) becomes another file via `Store.File(FileSpec)`. Shared
rules for every file:

* atomic write: temporary file in the same directory, fsync, rename,
  fsync of the directory; files 0600, the directory 0700 (created on the
  first write);
* no write if the bytes are unchanged (`state.json` has no "saved at"
  stamp for that reason; its mtime is logged instead);
* symbolic links at a file's path are never followed (Lstat +
  `O_NOFOLLOW`), neither for reading nor writing;
* envelope: a JSON object with `"version"`; an older version goes through
  `FileSpec.Migrate`;
* a damaged or unreadable file is renamed to `<name>.corrupt-<UTC time>`
  (the newest three are kept) before anything can overwrite it, logged,
  and hmbridge continues without the data (never blocks startup); a
  damaged devices file is also reported by the API;
* a file with a newer version (after a downgrade), a symlink, or a damaged
  file that cannot be moved aside is left alone and *blocks* writes
  (`File.Blocked`): for `devices.json` this disables pairing, unpairing
  and renaming, with the reason in the API (`pairing.disabled_reason`).

Compatibility: the deprecated `state_file` and `devices_file` (logged at
startup) still name exact paths (`""` disables that file); `state_dir`
applies to files they don't name. On the first start with a `state_dir`
other than the old default, `/var/lib/hmbridge/state.json` and
`devices.json` are copied into it if the new file does not exist yet (the
old files stay, and the copy is logged). The broker's retained messages
remain the primary source of state after a restart (`SeedFromRetained`).

**AES key exchange** (`bidcosaes.KeyExchangePayloads`): two messages, cmd
0x04, each `AES-ECB(old key, 01 idx·2+half key[8] rnd(2) 7E296FA5)`
(FHEM assignHmKey, Homegear generateKeyChangePacket). The RPI-RF-MOD
driver does AES in software, so hmbridge encrypts with the eQ-3 default key
(`Module.HostKeyExchange`). The HM-MOD-RPI-PCB gets the 2-byte templates
`01 <old idx·2>` / `01 <old idx·2+1>` and fills in its current key itself
(FHEM non-CUL branch, Homegear Hm_Mod_Rpi_Pcb). Key exchange payloads are
never logged (DualConn debug log, hmsniff).

**Verified vs. assumed.**

* Verified against FHEM's own code: the key exchange block layout and
  encryption (testdata/fhem_keyexchange.txt, generated by running FHEM's
  assignHmKey byte handling with Crypt::Rijndael; the AES handshake
  vectors as before).
* Consistent in FHEM and Homegear, not yet seen on the air from OpenCCU:
  the CONFIG_START/WRITE_INDEX/END layout, registers 02/0A/0B/0C, the
  unpair sequence, DEVICE_INFO layout, model IDs (also in eQ-3's
  descriptions).
* Assumed: AES_ACTIVE as list 1 register 0x08 bit 0 written to the device
  (Homegear's descriptions and FHEM's `sign` register; eQ-3's own rftypes
  mark AES_ACTIVE of the contacts as central-internal, so OpenCCU may not
  write it at all); writing 0x01 to the whole register (FHEM keeps the other
  bits, none are defined); that devices challenge the key exchange with
  their old key; that the HM-MOD-RPI-PCB moves a device from index 0
  directly to its current index > 1 (Homegear only allows old + 1); the
  INTERNAL_KEYS_VISIBLE bit (Homegear sets it, FHEM writes 0x01). An hmsniff
  capture of OpenCCU pairing a contact and a switch plug settles all of
  these (docs/DEPLOY.md “Adding devices”).

## Device settings

`internal/hm/params` describes the settings of the supported models (eQ-3's
MASTER parameter set: list 0 of the device, list 1 of the channels, list 7
of the thermostats), `internal/bridge/settings.go` reads and writes them,
`internal/ha/settings.go` exposes them. Sources: eQ-3's device descriptions
as shipped with Homegear (`misc/Device Description Files`: rf_cc_rt_dn.xml,
rf_tc_it_wm-w-eu.xml, rf_sec_sco.xml, rf_pb-2-wm55_*.xml, rf_rc-4-2.xml,
rf_s_644.xml, rf_dim_1l_644.xml, rf_ou_led16_*.xml) for register index,
size, scaling and range, cross-checked with FHEM (HMConfig.pm
culHmRegDefine/culHmRegModel; 10_CUL_HM.pm getConfig, pushConfig, the
RegisterRead and ParamChange parsing). Read as references only (GPL); every
setting names its sources in `params/models.go`.

**Model.** A setting is a bit field (`index.bit`, `bytes.bits` as in eQ-3's
notation) in a list (`params.Loc`: channel byte on the wire and list number)
with a kind: number (`raw = round((v − offset)·scale)`, range and step from
the description; eQ-3 enumerations of evenly spaced values such as the
temperature offset −3.5K=0 … 3.5K=14 or the boost period 0, 5 … 30 min are
numbers too), select (enumeration), switch (raw 1/0), or a week profile day
(text). The contact's event delay uses eQ-3's config time with factors 1
and 60 (bit 7 selects the factor; FHEM fltCvT60). A value is validated
before anything is sent (range, step, representability); out-of-range values
are rejected with the reason in the status.

The thermostats' list 7 is read and written with **channel 0** (FHEM:
shadow register “List 7 read from CH 0” for HM-CC-RT-DN and
HM-TC-IT-WM-W-EU; eQ-3 puts these parameters into the device's MASTER set).
**Week profile** (program 1 of the wall thermostat): registers 20 … 201,
26 per day in the order Saturday … Friday, 13 16-bit big-endian words per
day: bits 0-8 end time / 5 min, bits 9-14 temperature · 2. A day is
written completely; slots after the one ending at 24:00 are filled with
24:00 and the last temperature (the devices' defaults look like that).

**Protocol.**

| step | messages |
|---|---|
| read | CONFIG_PARAM_REQ `ch 04 00000000 list` (BiDi); the device answers with INFO_PARAM_RESPONSE `10 02 reg val …` (pairs, ended by `00 00`) or `10 03 start val …` (sequence, ended by start `00`), one frame per ≤ 8 pairs, each acknowledged by the central |
| write | CONFIG_START `ch 05 00000000 list`, CONFIG_WRITE_INDEX `ch 08 reg val …` (≤ 7 pairs each, only the registers that change), CONFIG_END `ch 06` (pairing.WriteListSequence) |
| verify | the list is read again and every change compared |
| device change | INFO_PARAM_CHANGE `10 04 ch 00000000 list pairs…` / `10 05 … start values…` updates a cached list (only when authenticated, or from a device without AES) |

The first answer frame is the response to the request (returned by
`Send`); the others arrive as received frames, which the event loop hands
to the worker waiting for them (`paramCollector`, one at a time) before the
AES checks; they never change device state. The HM-MOD-RPI-PCB acknowledges
them itself. `DualConn` acknowledges the INFO_PARAM_RESPONSE frames of a
peer for 30 s after a CONFIG_PARAM_REQ to it (at most 64) outside its
software ACK budget and without challenging them: a thermostat's list 7 is
about 26 frames, more than the challenge rate limit (10 per 10 s) allows.
Writes go through `hm.StandardDevice.Send`, so the ACK of a write to an AES
channel must be authenticated (`CheckReplyAuth`), like any command's.

**Jobs.** A settings job (`jobSettings`, key `settings:<addr>`, coalesced:
the queued job always carries all pending changes) reads the lists it needs
(all of the model's on refresh; otherwise only when a change shares a
register whose other bits are not cached), writes, reads back. The first
radio failure ends the job; retryable failures are retried twice like
commands. The worker's timeout for these jobs is 2 minutes. Pending changes
are kept per setting until the device confirmed them (`detail`: pending →
writing → ok / failed).

**Battery devices.** Thermostats (wake-on-radio): every message with the
burst flag (0xB0), like the climate commands and the unpairing. Contacts and
buttons (config-only): changes and refreshes wait; a DEVICE_INFO (config
button) or any message with the WAKEMEUP flag (0x02; FHEM processes its
command stack then) from the device queues them as a priority job without
burst. A failure that looks like “device asleep” keeps them pending. Pending
changes are not persisted.

**Cache.** `settings.json` (store, version 1): per device its model, the
time of the last change and the lists as hex address/value pairs. Written
only when the registers read differ from the cache (and on unpairing). A
cache of another model at the same address is ignored. Settings are never
read periodically: after pairing (while the device is still in config mode),
on “Refresh settings”, and before a write that needs unknown bits.

**Home Assistant.** Per setting a `number`/`select`/`switch`/`text` entity
(entity_category config; rarely used ones disabled by default) reading
`<base>/<addr>/settings` (`current.<key>`), commanding
`<base>/<addr>/set/setting_<key>`; a “Settings” sensor (status, attributes
`detail` and `read_at`) and a “Refresh settings” button. These entities use
the bridge's availability only, so changes can be queued for a sleeping
device. `set/setting_week_profile` takes several days as JSON. Entities of
settings a device does not have are removed once per start
(AllStaleDiscoveryMessages with `DeviceDesc.AllSettings`) and on unpairing.

**Verified vs. assumed.**

* Verified against two sources (eQ-3/Homegear and FHEM agree on index,
  size, scaling and range): every exposed setting; the tests use the values
  from eQ-3's descriptions (defaults, enumeration indexes).
* Left out because the sources disagree: GLOBAL_BUTTON_LOCK and
  MODUS_BUTTON_LOCK (Homegear writes 1, FHEM 200), LOCAL_RESET_DISABLE
  (Homegear bit 0 of register 24, FHEM the register with 200), the LED
  display's DISPLAY_ENERGYOPTIONS (0 … 255 vs. 0 … 127 with 0 = permanent),
  the contact's MSG_FOR_POS_A/B (positions 32.4/32.6 swapped). Not in the
  MASTER set: the radiator thermostat's display mode, the wall thermostat's
  window and decalcification settings, LED/status settings of the plugs,
  contacts and buttons, the dimmer's ramp times and levels (link
  parameters). The weather sensors only have BURST_RX.
* Assumed, not yet seen on the air: that the devices answer register reads
  with the frames above (FHEM's parsing); that a thermostat stays awake for
  the messages of a write when each carries the burst flag; whether devices
  sign INFO_PARAM_RESPONSE frames. hmbridge takes them unauthenticated (not
  failed) while it waits for them, from the device's address, to the
  central: someone who forges them in time can make hmbridge show wrong
  values or, for a register shared by several settings, write wrong values
  of the *other* bits along with an authenticated change. Read-back
  verification does not detect that (it reads the same way).
* The switch's POWERUP_ACTION “on” simulates a press of the device's
  internal button after power-up (FHEM: “simulate short press of peer
  self01”), so its effect depends on that button's (default) link.

## Running alongside OpenCCU

The module can only be owned by one process. During development the
CCU services are stopped, hmbridge runs, then the CCU is started again.
Device pairings are unaffected because hmbridge reuses the CCU's
address and key.

## Fresh installation without OpenCCU

On plain Raspberry Pi OS there is no raw-uart driver: hmbridge drives the
Pi's PL011 UART (`/dev/serial0` → `/dev/ttyAMA0`) and the module's reset
line itself (`cmd/hmbridge/uart.go`).

**Line settings.** `serial.Configure`: 115200 baud, 8N1, `CLOCAL|CREAD`, no
`CRTSCTS`, no software flow control, raw input/output, `VMIN=1 VTIME=0`. This
matches what OpenCCU's `pl011_raw_uart` driver programs into the UART (8N1
with FIFO, no CTS/RTS); the modules have no handshake lines. The port is
opened `O_NONBLOCK`, so Go's poller handles it: `Close` interrupts a pending
read, and reads can have deadlines (used by the probe below). A
`/dev/raw-uart` device is not a tty and is used as is.

**Reset lines.** OpenCCU's overlay `rpi-rf-mod.dts` lists
`pivccu,reset-gpios = <&gpio 18 0>, <&gpio 19 0>`: GPIO 18 for the
HM-MOD-RPI-PCB, GPIO 19 for the RPI-RF-MOD. The raw-uart driver
(`generic_raw_uart.c`) uses the second one when it finds the RPI-RF-MOD's
RTC (RX8130, I²C 0x32), and pulses the line low (50 ms low, 50 ms high, then
input). hmbridge (`internal/gpio`) holds it low for 150 ms, flushes the UART
input, drives it high and keeps the line requested, through the GPIO
character device (uAPI v2, v1 fallback). The chip is the one labelled
`pinctrl-bcm2835`/`-bcm2711`/`-bcm2712`/`-rp1` (`GPIO_GET_CHIPINFO_IOCTL`),
falling back to `/dev/gpiochip0`; on a Pi 5 the header's chip number depends
on the kernel version.

**Which line.** `reset_gpio` unset means the configured module's line
(`config.ResetGPIORPIRFMod` = 19, `DefaultResetGPIO` = 18). With `"radio":
"auto"`, `resetPin` decides before the reset: the RX8130 in sysfs
(`/sys/bus/i2c/devices/*-0032/name`, only with the RTC overlay) means
RPI-RF-MOD; otherwise `uartgw.ProbeKind` sends IDENTIFY (dst COMMON, the
counter `detectCnt`) and reads frames for up to 1 s with a read deadline: an
IDENTIFY answer or any dual-firmware destination means RPI-RF-MOD, a
classic ack/announcement means HM-MOD-RPI-PCB, silence falls back to GPIO
18. A wrong guess only leaves the module un-reset: `OpenModule` detects a
running application as well (the dual-firmware application is not rebooted
anyway, the classic `Conn` restarts its module itself). After the reset,
detection runs as before: the bootloader announcement (`Co_CPU_BL` /
`HMIP_TRX_Bl`), CHANGE_APP into `DualCoPro_App` for the RPI-RF-MOD. `reset`
`auto` resolves to `gpio` except for `/dev/raw-uart*` and for an RPI-RF-MOD
that is not on the Pi's own UART (e.g. a USB adapter).

**`hmbridge probe`** opens the port, resets and initializes the module like
the bridge (with central address `000001` without a configuration; the
module keeps it in RAM, nothing is transmitted), prints kind, application,
firmware, serial number and SGTIN, and closes it again.

**`hmbridge init`** writes a configuration without devices and a new
128-bit system key (crypto/rand) into `<state_dir>/system.key` as 32 hex
digits, mode 0600, owned by the service user. The configuration only
references it (`security.key_file`, `index` 1). Both files are created with
`store.WriteFileNew`: written and fsynced under a temporary name, then
hard-linked into place, which fails atomically if the name exists, so an
existing configuration or key is never overwritten. `config` reads
`key_file` at parse time (absolute path, regular file, at most 256 bytes,
the key never in an error message) and warns like for the configuration
file when the key file has any group/other permission bits or belongs to
another user. The central address is random in `100000`–`EFFFFF` (rejection
sampling, no modulo bias), avoiding `000000`, `FFFFFF` and, by convention,
the low and high ranges other software uses for broadcasts and virtual
devices.

**Re-pairing an AES device.** Unpairing only clears a device's central
address; its key stays. Pairing it again therefore cannot exchange the key
from the eQ-3 default key: the device challenges part 1 with its key index,
which the module (peer registered with index 0) refuses to answer, or the
device cannot decrypt the payload. `Bridge.pairKeyExchange` then retries
once per configured key (current, then previous) as the old key, after
registering the peer with that key's index. If part 1 of the retry with the
current key is authenticated, the device holds the system key whether or
not part 2 arrives.
