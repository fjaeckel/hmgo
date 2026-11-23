<!-- SPDX-License-Identifier: BSD-3-Clause -->

# Testing the gokrazy image on hardware

`hwtest_test.go` tests a Raspberry Pi running the gokrazy image
([../README.md](../README.md)) from another computer (Linux or macOS). It
uses the gokrazy API, hmbridge's HTTP API and MQTT, and changes nothing
unless a test is enabled explicitly. The QEMU acceptance test (`make
gokrazy-vmtest`) runs its non-interactive parts against the emulated
system, so the harness itself is tested.

The defaults are for a **Raspberry Pi 3 Model B+ with an RPI-RF-MOD**
(`HWTEST_MODEL`, `HWTEST_RADIO` change them).

```sh
export HWTEST_INSTANCE=~/gokrazy/hmbridge-debug    # the instance directory
go test -tags hwtest -v -count=1 -timeout 0 -run TestBringUp ./contrib/gokrazy/hwtest/
```

| test | what it does | enable with |
|---|---|---|
| `TestBringUp` | Model, hmbridge running, radio module kind, firmware, serial and SGTIN, reset line (GPIO 19 for the RPI-RF-MOD), central address, `/perm` in use, health, metrics, clock skew < 10 s, MQTT availability and discovery, radio reception | always |
| `TestPairedDevices` | every device: available, last seen, RSSI, AES state; fails on unavailable devices and failed AES | always |
| `TestPairing` | pairs devices one by one through MQTT and exercises each by model, asking you to press buttons, open windows, look at displays | `HWTEST_INTERACTIVE=1` |
| `TestReboot` | reboots through the gokrazy API without kexec (firmware and `config.txt` run each time); checks that devices, central address and key survived, from the log | `HWTEST_REBOOTS=10` |
| `TestUpdate` | `gok update` with a new version number, on a copy of the instance; checks the new version runs and nothing was lost | `HWTEST_UPDATES=5`, optionally `HWTEST_TESTBOOT=1` |
| `TestPowerCut` | cuts the power with a Home Assistant switch at random moments (during boot, during module setup, idle), then checks the last full boot | `HWTEST_POWER_CYCLES=30`, `HWTEST_POWER_SWITCH=switch.…`, `HA_URL`, `HA_TOKEN` |

Other variables: `HWTEST_HOST` (if the Pi's host name does not resolve),
`HWTEST_API` (hmbridge's API URL), `HWTEST_MQTT_BROKER` (if the broker
has another address from here than from the Pi), `GOK`.

## Runbook: Raspberry Pi 3 B+ with RPI-RF-MOD

**Hardware.** The RPI-RF-MOD sits on the header's first pins (pin 1 at the
SD card end), with the Pi powered by a 2.5 A supply. Use Ethernet, since
the image has no Wi-Fi configuration. The module talks to the PL011 UART
(`/dev/ttyAMA0`), which the image frees from Bluetooth
(`dtoverlay=disable-bt`). Its reset line is GPIO 19.

hmbridge has run well on this hardware on OpenCCU, with `/dev/raw-uart`
and `"reset": "none"`. That proves the module protocol: acknowledgements
and AES in software. The gokrazy image reaches the module differently,
and these parts are new on real hardware:

* **Serial port:** the standard PL011 driver (`/dev/ttyAMA0`, line set up
  by hmbridge) instead of eQ-3's raw-uart driver.
* **Reset and cold start:** hmbridge resets the module like OpenCCU's
  driver (50 ms low, 50 ms high, then the line released), then takes it
  from its bootloader into the application. On OpenCCU, OpenCCU had
  already started the module. The RPI-RF-MOD's reset is active high: the
  first version of this code held it in reset (fixed, verified on a Pi 3
  B+ with firmware 4.4.22).
* **Detection:** without the RTC overlay, hmbridge asks the module
  (IDENTIFY) which reset line to use. It answers in its bootloader and in
  its application. If no answer comes (a hung module), hmbridge resets
  GPIO 19 and listens for the RPI-RF-MOD's announcement before falling
  back to GPIO 18, the HM-MOD-RPI-PCB's line. `"radio": "rpi-rf-mod"` in
  `hmbridge.json` skips the question and always uses GPIO 19.

Every boot exercises all three, so the reboot and power-cut tests matter
most.

### 1. Baseline: your OpenCCU card

Keep the OpenCCU card that worked. On the same Pi with the same module,
swapping cards tells hardware problems apart from image problems. For a
direct comparison, give the gokrazy image the configuration that worked
on OpenCCU, so your devices work without pairing again:

* Replace the instance's `hmbridge.json` with your OpenCCU configuration,
  changing `"serial_port": "/dev/ttyAMA0"`, `"reset": "auto"` (or `"gpio"`),
  `"state_dir": "/perm/hmbridge"` and `"require_synced_clock": false`.
* The key is either a `passphrase`/`key` in that file (it is then part of
  the image), or a `key_file` at `/perm/hmbridge/system.key` that you copy
  there over SSH on the debug image (step 2) before hmbridge first starts
  with it.
* hmbridge refuses to create a new key while devices are configured, so a
  missing key file stops it instead of locking your devices out.

A Raspberry Pi OS card (docs/DEPLOY.md, steps 1–6) is another baseline
for the plain-UART path: `for i in $(seq 10); do sudo hmbridge probe; done`
must say `RPI-RF-MOD`, `GPIO 19` and `OK` every time.

### 2. Debug image

```sh
contrib/gokrazy/new-instance.sh -debug ~/gokrazy/hmbridge-debug
gok --parent_dir ~/gokrazy -i hmbridge-debug overwrite --full /dev/sdX
```

It adds SSH (`ssh hmbridge-debug`, breakglass with your public key, a
busybox shell), `hmbridge -debug` (every radio frame in the log) and the
header's I²C bus. Boot it. The first boot formats `/perm` and reboots
once. Then:

```sh
export HWTEST_INSTANCE=~/gokrazy/hmbridge-debug
go test -tags hwtest -v -count=1 -timeout 0 -run 'TestBringUp|TestPairedDevices' ./contrib/gokrazy/hwtest/
gok --parent_dir ~/gokrazy -i hmbridge-debug logs -s hmbridge    # the log, live
```

First check the startup lines in the log: `reset the radio module via
GPIO 19 … (RPI-RF-MOD reset line: the module answered IDENTIFY)` (or
`…: the module announced itself after a reset on it`), then
`radio module ready: RPI-RF-MOD, firmware …`. If detection picks GPIO 18
or the module does not come up, set `"radio": "rpi-rf-mod"` and try again
(`gok update`). That tells detection problems apart from reset problems.

On the Pi (breakglass), check:

* `ls -l /dev/ttyAMA0 /dev/gpiochip*`
* `cat /proc/device-tree/model`
* `ls /sys/bus/i2c/devices/` (bus 1 present; with busybox's `i2cdetect -y 1`, if your busybox has it, 0x32 is the module's RX8130)
* `ls -l /perm/hmbridge/` (`system.key` mode 0600)

### 3. Devices

```sh
HWTEST_INTERACTIVE=1 go test -tags hwtest -v -count=1 -timeout 0 -run TestPairing ./contrib/gokrazy/hwtest/
```

Pair one device of every model you have (factory-reset devices that were
paired to a CCU). For each one the test checks AES, commands with their
answers, settings (thermostats), events and the display. Then leave the
devices running. They are what the next steps check.

### 4. Reboots, updates, power cuts

```sh
HWTEST_REBOOTS=10 go test -tags hwtest -v -count=1 -timeout 0 -run TestReboot ./contrib/gokrazy/hwtest/
HWTEST_UPDATES=5  go test -tags hwtest -v -count=1 -timeout 0 -run TestUpdate ./contrib/gokrazy/hwtest/
HWTEST_UPDATES=2 HWTEST_TESTBOOT=1 go test -tags hwtest -v -count=1 -timeout 0 -run TestUpdate ./contrib/gokrazy/hwtest/
# The Pi on a smart plug in Home Assistant (a long-lived access token):
HWTEST_POWER_CYCLES=30 HWTEST_POWER_SWITCH=switch.pi_plug HA_URL=http://homeassistant.local:8123 HA_TOKEN=… \
  go test -tags hwtest -v -count=1 -timeout 0 -run TestPowerCut ./contrib/gokrazy/hwtest/
```

After these, run `TestPairedDevices` again and trigger each device once
(open a window, press a button). The devices must still work, with AES
`ok`. Then pull the plug by hand during a `gok update` once.

Also check by hand:

* MQTT broker stopped for a few minutes: hmbridge stays up and reconnects.
* Ethernet unplugged at boot: hmbridge waits for the clock, then starts once the network is back.
* The radio module removed and reseated with the power off.

### 5. Long-term run

Turn the debug instance into the production one: in its `config.json`,
remove `github.com/gokrazy/breakglass` and `github.com/gokrazy/serial-busybox`
from `Packages` (and breakglass's `PackageConfig`) and `-debug` from
hmbridge's flags, then `gok update`. `/perm` (key, devices) and the
central address stay. Back up `/perm/hmbridge/` with breakglass before
you remove it. Watch for one to two weeks with Prometheus and
[contrib/prometheus/hmbridge.rules.yml](../../prometheus/hmbridge.rules.yml):

* restarts (`hmbridge_build_info` gaps, `process_start_time_seconds`)
* radio errors and retries (`hmbridge_uartgw_*`)
* duty cycle
* devices going unavailable
* memory (`process_resident_memory_bytes`)

Run `TestBringUp` and `TestPairedDevices` now and then. The thermostats'
week programs must switch at the right time after a power cut (the clock,
see ../README.md).

### 6. Comparing with OpenCCU (if you have an OpenCCU card)

For the RPI-RF-MOD protocol, record what OpenCCU sends for the same
actions with `hmsniff` ([docs/SNIFFING.md](../../../docs/SNIFFING.md)) and
compare it with hmbridge's `-debug` frames: pairing, a thermostat command,
a window contact event with AES.

### When something fails

Keep:
* the test output (`-v`);
* `gok … logs -s hmbridge` from the debug image (every radio frame);
* `/api/v1/bridge` and `/api/v1/devices`;
* for boot problems, a screen on the HDMI port, where the kernel console is.
