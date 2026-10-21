# hmbridge design

hmbridge turns a Raspberry Pi with an HM-MOD-RPI-PCB radio module into a
small, single-purpose replacement for a HomeMatic CCU: it speaks classic
HomeMatic (BidCoS-RF) to thermostats and exposes them to Home Assistant via
MQTT discovery. It is a fork of github.com/stapelberg/hmgo (module path
github.com/fjaeckel/hmgo).

## Goals (prototype)

* Supported devices: HM-CC-RT-DN (radiator valve drive, “heating” package)
  and HM-TC-IT-WM-W-EU (wall thermostat, “thermal” package).
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

Non-goals for the prototype: pairing new devices, editing week programs,
Homematic IP, the dual-coprocessor firmware, HM-MOD-RPI-PCB firmware
updates.

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
| `internal/ha` | HA discovery payloads, state/command topics, MQTT client |
| `internal/bridge` | glue: registry, event loop, command handling, status page |
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
The dual-coprocessor firmware (used for Homematic IP) answers on dst
254/255 or announces `DualCoPro_App`; hmbridge must detect that and
fail with `uartgw.ErrDualCoProFirmware` and a message telling the user
to install the classic firmware.

Commands we use: OS 0x02 get firmware, 0x06 normal mode (leave update mode), 0x0A CSMA/CA (off by default, like FHEM/Homegear), 0x0B get
serial, 0x0E set time (uint32 BE unix time + int8 UTC offset in 30 min);
App 0x00 set HMID, 0x02 send, 0x03 set current key, 0x0F set previous
key, 0x0B set temp key (key payload: 16 key bytes + 1 index byte; 17
zero bytes clears a slot), 0x06 add peer (`addr(3) keyIndex wakeup 00`),
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

Time requests (cmd 0x3F) from the devices are answered with Homegear's
format (UTC offset + seconds since 2000), and the module's clock is set
every `time_sync_interval`.

## Running alongside OpenCCU

The module can only be owned by one process. During development the
CCU services are stopped, hmbridge runs, then the CCU is started again.
Device pairings are unaffected because hmbridge reuses the CCU's
address and key.
