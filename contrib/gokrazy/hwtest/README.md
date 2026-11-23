<!-- SPDX-License-Identifier: BSD-3-Clause -->

# Testing the gokrazy image on hardware

`hwtest_test.go` tests a Raspberry Pi running the gokrazy image
([../README.md](../README.md)) from your computer (Linux or macOS). It
talks to the Pi the way you would: through the gokrazy API, hmbridge's
HTTP API and MQTT. By default it only reads. Pairing, reboots, updates and
power cuts each run only when you enable them.

The QEMU test ([../vmtest/](../vmtest/README.md)) runs `TestBringUp`,
`TestPairedDevices` and `TestReboot` against its emulated system, so the
harness itself is tested in CI.

## Quick start

```sh
export HWTEST_INSTANCE=~/gokrazy/hmbridge     # your instance directory
go test -tags hwtest -v -count=1 -timeout 0 -run 'TestBringUp|TestPairedDevices' ./contrib/gokrazy/hwtest/
```

The test reads the instance directory to find the Pi:

* **from `config.json`:** the host name (or `Update.Hostname`) and the
  web interface password;
* **from `hmbridge.json`:** the MQTT broker and login, the central address,
  and hmbridge's HTTP port.

If the Pi's name does not resolve on your network, set `Update.Hostname`
in `config.json` (see [First boot](../README.md#first-boot)) or
`HWTEST_HOST`.

The defaults expect a **Raspberry Pi 3 Model B+ with an RPI-RF-MOD**.
For other hardware, set `HWTEST_MODEL` and `HWTEST_RADIO`.

## The tests

| test | checks | runs when | leaves behind |
|---|---|---|---|
| `TestBringUp` | the Pi's model; hmbridge running with `-create_key_file`; the radio module's kind, firmware, serial and SGTIN; the reset line in the log (GPIO 19 for an RPI-RF-MOD, if the line is still in gokrazy's log buffer); the central address; `/perm` in use; health; metrics (radio up, MQTT connected, permissions); the Pi's clock within 10 s of yours; MQTT availability and discovery; radio reception | always | nothing |
| `TestPairedDevices` | every device: heard since hmbridge started, available, AES not failed; prints last seen, RSSI, messages and AES per device | always | nothing |
| `TestPairing` | pairs devices one by one through MQTT, then exercises each by model (see below), with prompts on your terminal | `HWTEST_INTERACTIVE=1` | the devices stay paired |
| `TestReboot` | reboots through the gokrazy API, without kexec, so the Pi's firmware and `config.txt` run each time; after each reboot: hmbridge back and healthy, same devices and central address, no new key, no panic or damaged file in the log | `HWTEST_REBOOTS=<n>` | nothing; the bridge is offline for about 30 s per reboot |
| `TestUpdate` | `gok update` with a new version number, built from a copy of your instance (the instance directory is not changed); the same checks as after a reboot, plus the new version running | `HWTEST_UPDATES=<n>`, optionally `HWTEST_TESTBOOT=1` (`gok update --testboot`) | **a test build on the Pi**: run `gok -i <instance> update` afterwards to go back to your own |
| `TestPowerCut` | switches the Pi off and on with a Home Assistant switch, cutting the power at a random moment (during the boot, while the module is set up, or when idle), then checks the last full boot like a reboot | `HWTEST_POWER_CYCLES=<n>`, `HWTEST_POWER_SWITCH=switch.…`, `HA_URL`, `HA_TOKEN` | the switch on |

**Devices not heard yet.** Right after a start, `TestPairedDevices` only
reports devices it has not heard since hmbridge started: thermostats
report every few minutes, window contacts only on a change or about
hourly. It checks them from one hour after the start (`HWTEST_SETTLE`,
e.g. `30m`).

**What `TestPairing` does per model:**

| model | checks |
|---|---|
| window contacts and handle (HM-Sec-SC*, HM-Sec-RHS) | open and close, with AES |
| switch plug (HM-LC-Sw1-Pl-DN-R1) | on and off; asks whether it clicked |
| dimmer plug (HM-LC-Dim1L-Pl-3) | 50 %, then off; asks about the lamp |
| thermostats (HM-CC-RT-DN, HM-TC-IT-WM-W-EU) | target 21.5 (asks about the display), auto mode, a temperature report, reading the settings |
| buttons and remotes (HM-PB-2-WM55, HM-RC-8) | a short press of button 1 |
| LED display (HM-OU-LED16) | LED 1 red; asks whether it is |
| outdoor sensors (HM-WDS*) | a temperature report |

Use `TestPairing` only for devices that are not paired yet (factory-reset
devices that were paired to a CCU). Devices taken over from OpenCCU are
already in your configuration.

## Variables

| variable | meaning |
|---|---|
| `HWTEST_INSTANCE` | the instance directory (required) |
| `HWTEST_HOST` | the Pi's name or address, if it differs from `config.json` |
| `HWTEST_API` | hmbridge's API URL (default `http://<host>:8013`) |
| `HWTEST_MQTT_BROKER` | the broker as seen from your computer, if it differs from `hmbridge.json` |
| `HWTEST_MODEL` | text the Pi's model must contain (default `Raspberry Pi 3 Model B Plus`; empty: any) |
| `HWTEST_RADIO` | the module kind (default `RPI-RF-MOD`) |
| `HWTEST_SETTLE` | how long after hmbridge's start unheard devices are only reported (default `1h`) |
| `HWTEST_INTERACTIVE`, `HWTEST_REBOOTS`, `HWTEST_UPDATES`, `HWTEST_TESTBOOT`, `HWTEST_POWER_CYCLES`, `HWTEST_POWER_SWITCH`, `HA_URL`, `HA_TOKEN` | enable the tests above |
| `GOK` | the gok binary (default: from `PATH`) |

Use `-timeout 0`: the interactive and stress tests run longer than
`go test`'s default of 10 minutes.

## Runbook: Raspberry Pi 3 B+ with RPI-RF-MOD

### What is new compared to OpenCCU

hmbridge runs well on this hardware on OpenCCU, with `/dev/raw-uart` and
`"reset": "none"`. That proves the module protocol (acknowledgements and
AES in software). On gokrazy, hmbridge reaches the module differently:

* **Serial port:** the standard PL011 driver (`/dev/ttyAMA0`, line set up
  by hmbridge) instead of eQ-3's raw-uart driver.
* **Reset and start:** hmbridge resets the module itself and takes it from
  its bootloader into its application. On OpenCCU, OpenCCU did that.
* **Detection:** with `"radio": "auto"`, hmbridge finds out which module
  is attached.

Every boot exercises all three, so reboots and power cuts are the most
important tests. All three have worked on a Pi 3 B+ with firmware 4.4.22
(see [How it works](../README.md#how-it-works)).

### 1. Set up

Follow [Moving from OpenCCU](../README.md#moving-from-openccu) (or
[New installation](../README.md#new-installation)) with the debug image:

```sh
contrib/gokrazy/new-instance.sh -debug ~/gokrazy/hmbridge
```

The debug image logs every radio frame and has SSH access when you start
it (see [The debug image](../README.md#the-debug-image)). Keep the
OpenCCU card: on the same Pi with the same module, swapping cards tells
hardware problems apart from image problems.

### 2. Bring-up

```sh
export HWTEST_INSTANCE=~/gokrazy/hmbridge
go test -tags hwtest -v -count=1 -timeout 0 -run TestBringUp ./contrib/gokrazy/hwtest/
gok -i hmbridge logs -s hmbridge
```

In the log, the start must show:

1. `reset the radio module via GPIO 19 …`, with `the module answered
   IDENTIFY` or `the module announced itself after a reset on it`;
2. then `radio module ready: RPI-RF-MOD, firmware …`.

If the module does not come up, see
[Troubleshooting](../README.md#troubleshooting).

On the Pi (start breakglass in the web interface, then `ssh hmbridge`):

* `cat /proc/device-tree/model`
* `cat /proc/device-tree/aliases/serial0`, which must print `/soc/serial@7e201000`, the PL011
* `ls -l /dev/ttyAMA0 /dev/gpiochip*`
* `ls -l /perm/hmbridge/` (`system.key`, if any, mode 0600)

### 3. Devices

An hour after the start, run `TestPairedDevices`: every device must have
been heard, with no failed AES. Change something on one device of each
kind in Home Assistant (a thermostat's temperature, a plug) and trigger
the others (open a window, press a button). Pair new devices with
`HWTEST_INTERACTIVE=1 … -run TestPairing`.

### 4. Reboots, updates, power cuts

```sh
HWTEST_REBOOTS=10 go test -tags hwtest -v -count=1 -timeout 0 -run TestReboot ./contrib/gokrazy/hwtest/
HWTEST_UPDATES=3  go test -tags hwtest -v -count=1 -timeout 0 -run TestUpdate ./contrib/gokrazy/hwtest/
gok -i hmbridge update        # back to your own build after TestUpdate

# With the Pi on a smart plug in Home Assistant (a long-lived access token):
HWTEST_POWER_CYCLES=30 HWTEST_POWER_SWITCH=switch.pi_plug HA_URL=http://homeassistant.local:8123 HA_TOKEN=… \
  go test -tags hwtest -v -count=1 -timeout 0 -run TestPowerCut ./contrib/gokrazy/hwtest/
```

Each reboot or update takes the bridge offline for about 30 s; your
devices keep running on their own. Afterwards, run `TestPairedDevices`
again and trigger each device once.

By hand:

* pull the plug during a `gok update`;
* stop the MQTT broker for a few minutes: hmbridge stays up and reconnects;
* boot with Ethernet unplugged: hmbridge starts once the clock is set and
  connects once the network is back;
* remove and reseat the module with the power off.

### 5. Long-term run

Turn the debug instance into a normal one (see
[The debug image](../README.md#the-debug-image)). Back up the key first if
hmbridge created it. Then watch for one to two weeks with Prometheus
(`http://hmbridge:8013/metrics`) and
[contrib/prometheus/hmbridge.rules.yml](../../prometheus/hmbridge.rules.yml):

* restarts (`process_start_time_seconds`)
* radio errors and retries (`hmbridge_uartgw_*`)
* duty cycle
* devices going unavailable
* memory (`process_resident_memory_bytes`)

Run `TestBringUp` and `TestPairedDevices` now and then. After a power
cut, the thermostats' week programs must switch at the right time (see
[Clock](../README.md#clock)).

### 6. Comparing with OpenCCU (optional)

To compare the radio protocol, record what OpenCCU sends for the same
actions with `hmsniff` ([docs/SNIFFING.md](../../../docs/SNIFFING.md)).
Compare it with hmbridge's `-debug` frames for pairing, a thermostat
command, and a window contact event with AES.

## When something fails

Keep:

* the test output (`-v`);
* `gok -i <instance> logs -s hmbridge` from the debug image (every radio frame);
* `http://<pi>:8013/api/v1/bridge` and `/api/v1/devices`;
* for boot problems, what the HDMI screen shows (the kernel console).
