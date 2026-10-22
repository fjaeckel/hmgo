<!-- SPDX-License-Identifier: BSD-3-Clause -->

# Recording OpenCCU's radio traffic with hmsniff

`hmsniff` records what OpenCCU and its radio module (RPI-RF-MOD or
HM-MOD-RPI-PCB) say to each other, and decodes it. OpenCCU keeps running
normally: hmsniff only switches on the traffic dump of OpenCCU's raw-uart
kernel driver and reads the kernel log. It never opens the radio device and
never transmits anything.

The recording is what hmbridge's support for the RPI-RF-MOD is built from:
how OpenCCU configures the module, how messages are sent and acknowledged, and
what every one of your devices reports.

## Run it

1. Check the Pi's architecture on OpenCCU: `uname -m`
   (`aarch64` → `hmsniff-<version>-linux-arm64`, `armv7l` →
   `hmsniff-<version>-linux-armv7`). Download it from the project's GitHub
   releases (a tagged release, or the `nightly` pre-release for the latest
   build of master), or build it with `make dist` (into `dist/`).
2. Copy the binary to OpenCCU's `/tmp` (RAM, nothing is written to the SD card),
   naming the exact file (with `dist/` in front if you built it):

   ```sh
   scp hmsniff-<version>-linux-arm64 root@<openccu-ip>:/tmp/hmsniff   # or hmsniff-<version>-linux-armv7
   ```

3. On OpenCCU:

   ```sh
   chmod +x /tmp/hmsniff
   /tmp/hmsniff -duration 30m
   ```

   It prints decoded traffic as it happens and writes everything to
   `/tmp/hmsniff-<time>.log`. Stop it early with Ctrl-C. Either way it switches
   the traffic dump off again.

4. While it runs, do a few things so the recording covers them:
   * change a thermostat's target temperature in the OpenCCU web UI
   * open and close a window with a contact
   * press a wall button and switch a plug
5. Copy the log back: `scp root@<openccu-ip>:/tmp/hmsniff-*.log .`

## What is (not) in the log

* Key material is never written to hmsniff's own log. Frames that look like an AES key being
  loaded into the module are replaced by `<… redacted …>`.
* AES responses of the devices are redacted too (with the matching challenge,
  they would allow an offline attack on a weak key passphrase). `-no-redact`
  keeps them.
* The AES key exchange of a pairing (BidCoS cmd 0x04) is always redacted, also
  with `-no-redact`: it carries the system security key, encrypted with the
  device's old key, which for a new device is eQ-3's publicly known default
  key. hmsniff only shows the new key index and which half is sent. The same
  applies to the kernel log (next point): don't share `dmesg` from a recording
  in which a device was paired.
* Device types and serial numbers come from OpenCCU's `/etc/config/rfd/*.dev`;
  your device names (from the web UI) are not included.
* The kernel's own log is **not** redacted: while the raw-uart dump is on, the
  driver logs every frame to the kernel log (`dmesg`, and syslog if it keeps
  kernel messages), including frames that load an AES key into the module,
  e.g. when rfd or multimacd (re)starts. Keep recordings short, don't share
  `dmesg` output or syslog files from that time, and reboot (or clear the kernel
  log with `dmesg -c`) afterwards if a key may have been loaded meanwhile.

## Good to know

* The kernel log gets busy while the dump is on; this is harmless. If hmsniff
  is killed hard (`kill -9`), switch the dump off yourself:
  `echo 0 > /sys/class/raw-uart/raw-uart/dump_traffic`. A reboot also resets it.
* `hmsniff -replay <file>` decodes a saved kernel log (e.g. `dmesg` output)
  instead of recording.
* Do **not** restart OpenCCU's `multimacd` while recording: rfd and
  HMIPServer depend on it, and on an RPI-RF-MOD system a lone multimacd restart
  can end in a reboot of the whole box (which also wipes `/tmp`, including the
  log). A plain recording while OpenCCU runs normally is all that is needed.
* Copy the log off the box before rebooting it: `/tmp` is in RAM.
