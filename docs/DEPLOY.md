# Deploying hmbridge

hmbridge needs exclusive access to the HM-MOD-RPI-PCB radio module. While
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

## 2. Configuration

Copy `internal/config/example.json` to `/etc/hmbridge/config.json` and fill
in `central_address`, the MQTT broker of your Home Assistant installation and
one entry per device. Start with one device and add the rest once it works.

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
writable `"state_file"` such as `"/usr/local/hmbridge/state.json"` (or `""`:
the state is restored from the MQTT broker anyway). To
return to OpenCCU, simply reboot the Pi.

## 3b. Running on Raspberry Pi OS (final setup)

On a Raspberry Pi OS Lite installation:

1. `/boot/firmware/config.txt`: `enable_uart=1` and `dtoverlay=disable-bt`
   (Pi 3/4/Zero 2: gives the module the full PL011 UART on GPIO 14/15).
2. Remove `console=serial0,115200` from `/boot/firmware/cmdline.txt` and run
   `sudo systemctl disable --now hciuart`.
3. Install the binary to `/usr/local/bin/hmbridge`, the config to
   `/etc/hmbridge/config.json`, and `contrib/systemd/hmbridge.service` to
   `/etc/systemd/system/`. Create the user:
   `sudo useradd --system --no-create-home --groups dialout,gpio hmbridge`.
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
Prometheus can scrape them. They contain no secrets, but they do show device
names and addresses; set `"http_listen": "127.0.0.1:8013"` to keep them local.

## 4. Home Assistant

Make sure the MQTT integration is set up in Home Assistant. Each thermostat
appears as a device with a climate entity and sensors as soon as hmbridge
connects. A device that stops reporting (empty battery, out of range) becomes
unavailable in Home Assistant after its `stale_after` time (30 minutes for
thermostats, 6 hours for the HM-Sec-SCo, 50 hours for other contacts); its
"Last seen" sensor stays visible.

After a restart or reboot, hmbridge reads its last published state back from
the MQTT broker, so Home Assistant isn't blank until every window contact has
reported again; this needs no writes on the Pi. As a fallback for when the
broker is unreachable at startup, it also keeps `/var/lib/hmbridge/state.json`.
To spare the SD card, that file is only written after significant changes
(mode, target temperature, window, battery, fault), at most every 15 minutes
(`state_save_interval`), and on shutdown; ordinary temperature reports never
cause a write. The status page on port 8013 lists configured devices and every
radio address that talked to the central but is not configured — handy for
finding device addresses.

## Module firmware

OpenCCU may have installed eQ-3's "dual coprocessor" firmware on the module so
that it can talk Homematic IP. hmbridge (like FHEM and Homegear) only speaks
the classic firmware's protocol. Downgrading is a deliberate step; do it only
once you know which firmware is installed. Note that OpenCCU may upgrade the
module again when it starts with the radio module attached.
