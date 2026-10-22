<!-- SPDX-License-Identifier: BSD-3-Clause -->

# Homematic IP

hmbridge does **not** support Homematic IP (HmIP) devices yet. This page says
what the first step (“phase 1”) does, what is known about the protocol from
recordings, and what full support would take.

## Phase 1: receive and show, nothing else

Only on the RPI-RF-MOD (eQ-3's "DualCoPro" firmware, which carries classic
Homematic and HmIP over one serial line):

* `internal/uartgw` passes every frame the module sends on its HmIP
  destinations (0x02, and 0x0c, see below) to an optional handler
  (`Config.HMIPHandler`). The handler runs on its own goroutine with a bounded
  queue, so it never delays BidCoS traffic; frames are dropped (and counted as
  `hmbridge_uartgw_dual_events_total{event="hmip_dropped"}`) when it falls
  behind.
* `internal/hmip` decodes the HmIP serial commands and responses, and the MAC
  header of the radio frames they carry (source/destination address, content
  type, security control, security number, integrity code, flags) plus the
  application header (frame type, sequence number). No payload semantics, no
  encryption, no keys.
* hmbridge logs every decoded HmIP frame with `-debug`, counts them in
  `hmbridge_hmip_frames_total{kind="device"|"own"|"other"|"malformed"}`, and
  lists the radio addresses of frames from other devices in the HTTP API
  (`GET /api/v1/unknown`, field `hmip_heard`), with last seen and the RSSI the
  module measured. Nothing is published to MQTT.
* `hmsniff` decodes OpenCCU's HmIP traffic the same way instead of printing
  only command and length. Key material (network key, inclusion data, the
  0x0c exchange) is always redacted.

**hmbridge never sends anything on the HmIP destinations**: no address, no
key, no link partner, no answer. That is also why `hmip_heard` will
most likely stay empty: when hmbridge owns the module, nobody gives the module
its HmIP address, security counter and network key (OpenCCU's HMIPServer does
that at every start, see below). Whether the module then passes on any frames
from HmIP devices at all is unknown. Under OpenCCU, `hmsniff` shows them.

## What the recordings show

Three hmsniff recordings of OpenCCU (HMIPServer) with an RPI-RF-MOD,
firmware 4.4.22, two of them across an HMIPServer restart. The HmIP frames
are in `internal/hmip/testdata/captures.txt` (key material left out).

At its start HMIPServer sends, in this order (counter-matched responses from
the module in brackets):

| Command | Data | Response |
|---|---|---|
| 0x00 set address | its own radio address (3 bytes), e.g. `B4E47A` | OK (01) |
| 0x0d (CSMA/send attempt defaults) | `03` | OK |
| 0x19 (adapter …) | none | OK + 16 bytes, different at every start |
| 0x0a get security counter | none | OK + 4-byte counter |
| 0x08 set security counter | counter, moved ahead (e.g. `4D2F954F` → `4D2FBFC7`) | OK |
| 0x14 set encrypted network key | 66 bytes, identical at both restarts | OK |
| dst 0x0c: 0x02, 0x0e (75 bytes), 0x09 | | module answers with cmd 0x03, status 01 (0x09: plus 16 bytes, identical at both restarts) |
| 0x12 get link partners | none | OK + one address, `47DEF5` |

Then it transmits a few frames to the group addresses `F00001`/`F00002`
(content types 2 and 3, and an application frame of type 34 “trigger
status” with burst mode 3).

Decoded without problems: all serial envelopes and CRCs, every command and
response above, all MAC headers, the application header. Not decodable from
the available information: the payload of content type 2/3 frames (e.g.
`88 c0`, `88 04`), the 0x0c destination, and the meaning of commands 0x0d
and 0x19.

The only frame from another radio: `47DEF5` → `F00001`, content type 2,
secured, RSSI −69 dBm, i.e. the module's one link partner. So there is an HmIP
device (or a leftover pairing) in OpenCCU's configuration after all — worth
checking in OpenCCU's device list.

### Where the specification and the recordings differ

The protocol facts come from the community specification
[FrameShiftLab/hmip-protocol-specs](https://github.com/FrameShiftLab/hmip-protocol-specs)
(commit 90a4c2d; mainly `docs/serial-protocol.md` and
`docs/mac-application-frames.md`). It marks statements as verified on the
wire or derived; `internal/hmip` uses the verified ones. Against the
recordings and hmbridge's own working implementation:

* **Destinations.** The specification says 0x01 is the classic BidCoS
  subsystem. On the DualCoPro firmware 0x01 is the TRX/“APP” destination
  (firmware version, status polls) and BidCoS runs on 0x03 (LLMAC).
  Destination 0x0c is used by HMIPServer but not mentioned at all.
* **Byte stuffing.** The specification says an escaped byte is sent as 0xFC
  followed by the original byte. The module (and FHEM, and hmbridge) send
  0xFC followed by the byte with its top bit cleared (0xFD → `FC 7D`,
  0xFC → `FC 7C`). The recordings' raw lines are already unescaped, so
  they cannot show this, but hmbridge's reader only works with the latter.
* **CRC.** No difference: the specification's “init 0xFFFF over length…data”
  is the same as hmbridge's “init 0xD77F including the leading 0xFD”; all 82
  captured HmIP frames check out either way.
* **Received events are not only received frames.** Command 0x07 (received
  event) also reports every frame the host had the module transmit, with
  RSSI byte 0x80 (no reading). The first report has the security fields
  zeroed; for burst mode 3 two further reports follow with the real security
  number and integrity code and the QoS bit set.
* **Security number.** The specification treats it as opaque. In the
  recordings it is the module's security counter: HMIPServer sets it with
  command 0x08 and the following transmissions use the next values.
* **Status codes.** Both OK codes occur: 0x09 answers a send, 0x01
  everything else.
* **RSSI byte.** The specification calls it signed and says to negate it;
  for the observed values (0x45 = −69 dBm) this equals the BidCoS convention
  of negating the unsigned byte, which hmbridge uses. 0x80 means “no
  reading”.
* **Command 0x00** (set address) is listed without data; it carries the
  host's 3-byte address.
* Internal inconsistency of the specification: the network management frame
  type of an inclusion request is `0x10` in its worked example, `0x01` in
  another section. `internal/hmip` uses the worked examples (0x10 inclusion
  request, 0x20 inclusion accept, 0xf0/0xf2 removal).

## What full support would need

Every step below transmits, so none of it is in phase 1.

1. **A network key and the devices in the module.** HmIP frames are
   authenticated (and, for some content, encrypted) by the module, which
   needs the network key (command 0x14), the security counter (commands
   0x0a/0x08; it must never go backwards, so it has to be persisted) and its
   link partner table (commands 0x04/0x05/0x12). There are two ways to get
   there:
   * **Pairing with hmbridge itself.** The device sends an inclusion request;
     the module computes key exchange material from its own secret hardware
     key (command 0x11); for retail devices this is then sent to eQ-3's
     key server (`secgtw.homematic.com`), which returns the device's
     encrypted network key; the host registers the device (0x04) and sends
     the inclusion accept frame with the right burst mode. The specification
     reports this as working against the real service, but it requires the
     module's genuine access point identity (SGTIN) and depends on eQ-3's
     cloud service and its terms. A local-only variant with the per-device
     key printed on the device's packaging is derived from vendor software
     and **unverified**.
   * **Importing OpenCCU's pairing.** The module keeps its link partners, but
     HMIPServer re-sends the encrypted network key and the security counter
     at every start, so hmbridge would need HMIPServer's stored copy of both
     (plus the device list and addresses). The specification does **not**
     say where OpenCCU/HMIPServer stores them. Presumably below
     `/etc/config/crRFD/` (HMIPServer's data directory on OpenCCU) — this is
     **unverified** and needs to be checked on a real system. Whether the
     66-byte blob of command 0x14 is portable between hosts (it is encrypted;
     probably for this module only) is also unknown.
2. **Answering devices**: acknowledgements (application frame type 2) for
   frames with “response requested”, time info answers, and the
   specification's transmission discipline (one transmission in flight,
   retry on BUSY, burst mode per device listener mode).
3. **Decoding status frames per device type** and publishing them to Home
   Assistant, then commands (switch, level, set point) and configuration
   frames.

## Roadmap

1. Phase 1 (this): receive, decode headers, show heard devices. Done.
2. Record OpenCCU with a real HmIP device (hmsniff): status frames, answers,
   time requests, and a pairing — to check the specification's application
   layer against this firmware and to find out what HMIPServer stores.
3. Read-only operation with a pairing imported from OpenCCU: load key and
   counter at start, decode status frames, publish to MQTT. First
   transmissions on the HmIP destination (setup commands only).
4. Answers and commands for selected device types.
5. Own pairing (key server or local keys), if still wanted.

## Licensing

The specification's documentation is licensed CC BY-NC-SA 4.0; its code
snippets and pseudocode MIT/CC0. hmbridge (BSD-3-Clause) contains no text or
tables from it: `internal/hmip` is an own implementation of the protocol
facts it reports (bit layouts, command and type numbers), checked against
our own recordings, and cites it as a source. The specification's status
frame example bytes are used as a test vector. Homematic IP is a trademark of
eQ-3 AG; neither hmbridge nor the specification is affiliated with eQ-3.
