// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

//go:build vmtest

// Package vmtest is the acceptance test of the gokrazy image
// (contrib/gokrazy): it builds a gokrazy instance made by
// new-instance.sh -vm, boots it in QEMU with an emulated HM-MOD-RPI-PCB
// on the virtio console (/dev/hvc0, the Pi's UART in the real image)
// and a mosquitto broker, plays Home Assistant via MQTT, pairs a device,
// then updates the running system over the network (gok update: A/B
// root partitions, reboot) and checks that the key and the paired
// device survived.
//
// Run it with make gokrazy-vmtest (in a container with QEMU, mosquitto
// and gok), or on a Linux host that has them:
//
//	go test -tags vmtest -v -timeout 30m ./contrib/gokrazy/vmtest/
//
// Environment: GOK (gok binary, default from PATH), QEMU_EFI (arm64
// UEFI firmware, default /usr/share/qemu-efi-aarch64/QEMU_EFI.fd),
// VMTEST_KEEP=1 keeps the work directory (console log, image).
package vmtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/fjaeckel/hmgo/internal/hmkey"
	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

const (
	instance = "hmbridge-vm"
	pkg      = "github.com/fjaeckel/hmgo/cmd/hmbridge"
	// bootTimeout covers the first boot (mkfs on /perm and a reboot)
	// under QEMU without hardware acceleration.
	bootTimeout = 6 * time.Minute
)

func TestGokrazyImage(t *testing.T) {
	gok := lookTool(t, "GOK", "gok")
	qemu := lookTool(t, "", "qemu-system-aarch64")
	mosquitto := lookTool(t, "", "mosquitto")
	efi := os.Getenv("QEMU_EFI")
	if efi == "" {
		efi = "/usr/share/qemu-efi-aarch64/QEMU_EFI.fd"
	}
	if _, err := os.Stat(efi); err != nil {
		t.Skipf("arm64 UEFI firmware: %v (set QEMU_EFI)", err)
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	if os.Getenv("VMTEST_KEEP") != "" {
		work, err = os.MkdirTemp("", "hmbridge-vmtest-")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("work directory (kept): %s", work)
	}

	// Home Assistant's side: a broker on the host, reachable from the
	// guest as 10.0.2.2 (QEMU user networking).
	brokerPort := freePort(t)
	conf := filepath.Join(work, "mosquitto.conf")
	os.WriteFile(conf, []byte(fmt.Sprintf("listener %d 0.0.0.0\nallow_anonymous true\npersistence false\n", brokerPort)), 0o644)
	startProcess(t, filepath.Join(work, "mosquitto.log"), mosquitto, "-c", conf)
	waitTCP(t, fmt.Sprintf("127.0.0.1:%d", brokerPort))
	ha := newFakeHA(t, fmt.Sprintf("tcp://127.0.0.1:%d", brokerPort))

	// The gokrazy instance.
	parent := filepath.Join(work, "gokrazy")
	instDir := filepath.Join(parent, instance)
	run(t, repo, []string{"MQTT_BROKER=" + fmt.Sprintf("tcp://10.0.2.2:%d", brokerPort)},
		filepath.Join(repo, "contrib/gokrazy/new-instance.sh"), "-vm", instDir)
	updatePort := freePort(t)
	editConfig(t, instDir, func(c map[string]any) {
		// gok update reaches the guest through a forwarded port.
		u := c["Update"].(map[string]any)
		u["Hostname"] = "127.0.0.1"
		u["HTTPPort"] = fmt.Sprint(updatePort)
	})
	setVersion := func(v string) {
		editConfig(t, instDir, func(c map[string]any) {
			pc := c["PackageConfig"].(map[string]any)[pkg].(map[string]any)
			pc["GoBuildFlags"] = []string{"-ldflags=-X github.com/fjaeckel/hmgo/internal/buildinfo.version=" + v}
		})
	}
	setVersion("vmtest-1")

	img := filepath.Join(work, "disk.img")
	run(t, repo, nil, gok, "--parent_dir", parent, "-i", instance, "overwrite", "--full", img, "--target_storage_bytes=1400000000")

	// The emulated radio module, on the virtio console. QEMU keeps the
	// connection across guest reboots, so the module (and what
	// hmbridge configured in it) persists like the real one.
	dev := fakemod.NewDevice([3]byte{0x4c, 0x5d, 0x70}, 0x00c7, "NEQ0000002") // HM-Sec-SCo
	radio := startRadio(t, fakemod.Options{StartInApp: true, OnSend: dev.HandleClassic})

	apiPort := freePort(t)
	accel := []string{"-cpu", "max"}
	if _, err := os.Stat("/dev/kvm"); err == nil && runtime.GOARCH == "arm64" {
		accel = []string{"-accel", "kvm", "-cpu", "host"}
	}
	qemuArgs := append([]string{
		"-machine", "virt,highmem=off", "-m", "1024", "-smp", "2",
		"-bios", efi,
		"-drive", "file=" + img + ",format=raw,if=none,id=hd",
		"-device", "virtio-blk-pci,drive=hd",
		"-netdev", fmt.Sprintf("user,id=net0,hostfwd=tcp:127.0.0.1:%d-:%d,hostfwd=tcp:127.0.0.1:%d-:8013", updatePort, updatePort, apiPort),
		"-device", "virtio-net-pci,netdev=net0",
		"-chardev", fmt.Sprintf("socket,id=radio,host=127.0.0.1,port=%d", radio.port),
		"-device", "virtio-serial-pci",
		"-device", "virtconsole,chardev=radio",
		"-serial", "file:" + filepath.Join(work, "console.log"),
		"-display", "none", "-monitor", "none",
	}, accel...)
	startProcess(t, filepath.Join(work, "qemu.log"), qemu, qemuArgs...)
	t.Cleanup(func() {
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(work, "console.log"))
			t.Logf("guest console (last 6000 bytes):\n%s", tail(b, 6000))
		}
	})

	// First boot: gokrazy formats /perm (mkfs) and reboots; hmbridge
	// creates the key in /perm and sets up the module.
	waitFor(t, bootTimeout, "hmbridge online", func() bool { return ha.get("hmbridge/bridge/availability") == "online" })
	waitFor(t, time.Minute, "system key in the module", func() bool { return len(radio.mod().Key(fakemod.SlotCurrent)) == 17 })
	key1 := radio.mod().Key(fakemod.SlotCurrent)
	if bytes.Equal(key1[:16], hmkey.EQ3Default.Bytes[:]) || key1[16] != 1 {
		t.Fatalf("module key: default key or index %d, want a new key with index 1", key1[16])
	}
	central, ok := radio.mod().HMID()
	if !ok || central != centralOf(t, instDir) {
		t.Errorf("module HMID %X, want the configuration's central address", central)
	}
	waitFor(t, time.Minute, "pair button discovery", func() bool {
		return ha.json("homeassistant/button/hmbridge_bridge/pair/config")["command_topic"] == "hmbridge/bridge/set/pair"
	})

	api := fmt.Sprintf("http://127.0.0.1:%d", apiPort)
	var health struct {
		Status string `json:"status"`
	}
	waitFor(t, time.Minute, "health ok", func() bool {
		return getJSON(api+"/api/v1/health", &health) == nil && health.Status == "ok"
	})
	metrics := httpGet(t, api+"/metrics")
	for _, want := range []string{`hmbridge_build_info{goversion="`, `version="vmtest-1"} 1`, "hmbridge_uartgw_up 1", "hmbridge_mqtt_connected 1", "hmbridge_config_insecure_permissions 0"} {
		if !strings.Contains(metrics, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}

	// Pair a window contact from Home Assistant. The contact gets the
	// key generated on the device.
	dev.ModuleKey = &hmkey.Key{Index: key1[16]}
	copy(dev.ModuleKey.Bytes[:], key1[:16])
	ha.publish("hmbridge/bridge/set/pair", "PRESS")
	waitFor(t, time.Minute, "pairing mode", func() bool { return ha.json("hmbridge/bridge/pairing")["active"] == true })
	radio.mod().InjectRecv(0x01, dev.DeviceInfo(0x01, [3]byte{}))
	waitFor(t, 2*time.Minute, "paired", func() bool {
		r, _ := ha.json("hmbridge/bridge/pairing")["last_result"].(string)
		return strings.HasPrefix(r, "paired HM-Sec-SCo NEQ0000002")
	})
	if k := dev.Key(); k.Bytes != dev.ModuleKey.Bytes {
		t.Errorf("the paired device did not get the system key")
	}
	radio.mod().InjectRecv(0x02, dev.Message(0x20, 0xa4, 0x41, central, 0x01, 0x05, 0xc8))
	waitFor(t, time.Minute, "window open", func() bool { return ha.json("hmbridge/4c5d70/state")["window_open"] == true })

	// The hardware test (contrib/gokrazy/hwtest) against this VM: its
	// checks of a running system, the paired device and a reboot.
	t.Run("hwtest", func(t *testing.T) {
		out, err := runErr(repo, []string{
			"HWTEST_INSTANCE=" + instDir,
			"HWTEST_HOST=127.0.0.1",
			fmt.Sprintf("HWTEST_API=%s", api),
			fmt.Sprintf("HWTEST_MQTT_BROKER=tcp://127.0.0.1:%d", brokerPort),
			"HWTEST_MODEL=", "HWTEST_RADIO=HM-MOD-RPI-PCB", "HWTEST_REBOOTS=1",
		}, "go", "test", "-tags", "hwtest", "-count=1", "-v", "-run", "TestBringUp|TestPairedDevices|TestReboot", "./contrib/gokrazy/hwtest/")
		t.Logf("%s", out)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"--- PASS: TestBringUp", "--- PASS: TestPairedDevices", "--- PASS: TestReboot"} {
			if !strings.Contains(out, want) {
				t.Errorf("hwtest output lacks %q", want)
			}
		}
	})

	// Update over the network: new root file system, reboot.
	setVersion("vmtest-2")
	ha.forget("hmbridge/4c5d70/state")
	framesBefore := len(radio.mod().Frames())
	run(t, repo, nil, gok, "--parent_dir", parent, "-i", instance, "update")
	waitFor(t, bootTimeout, "updated hmbridge running", func() bool {
		b, err := httpGetErr(api + "/metrics")
		return err == nil && strings.Contains(b, `version="vmtest-2"} 1`)
	})
	waitFor(t, time.Minute, "hmbridge online after the update", func() bool { return ha.get("hmbridge/bridge/availability") == "online" })
	// The new hmbridge set up the module again, with the key it read
	// from /perm: a lost /perm would have made it create a new one.
	if n := len(radio.mod().Frames()); n <= framesBefore {
		t.Errorf("the module got no frames after the update")
	}
	if key2 := radio.mod().Key(fakemod.SlotCurrent); !bytes.Equal(key1, key2) {
		t.Errorf("the system key changed across the update")
	}
	// The paired device is still known: its next event arrives.
	radio.mod().InjectRecv(0x02, dev.Message(0x21, 0xa4, 0x41, central, 0x01, 0x06, 0x00))
	waitFor(t, time.Minute, "window closed after the update", func() bool {
		v, ok := ha.json("hmbridge/4c5d70/state")["window_open"]
		return ok && v == false
	})
	var devs []map[string]any
	if err := getJSON(api+"/api/v1/devices", &devs); err != nil || len(devs) != 1 {
		t.Errorf("/api/v1/devices after the update: %v %v", err, devs)
	}
}

// TestRaspberryPiImage builds the Raspberry Pi image made by
// new-instance.sh and checks its boot configuration: the radio module's
// UART is enabled and free of the kernel console.
func TestRaspberryPiImage(t *testing.T) {
	gok := lookTool(t, "GOK", "gok")
	mtype := lookTool(t, "", "mtype")
	repo, _ := filepath.Abs("../../..")
	work := t.TempDir()
	parent := filepath.Join(work, "gokrazy")
	instDir := filepath.Join(parent, "hmbridge")
	run(t, repo, []string{"MQTT_BROKER=tcp://homeassistant.local:1883", "MQTT_USERNAME=hmbridge", "MQTT_PASSWORD=x"},
		filepath.Join(repo, "contrib/gokrazy/new-instance.sh"), instDir)
	img := filepath.Join(work, "rpi.img")
	run(t, repo, nil, gok, "--parent_dir", parent, "-i", "hmbridge", "overwrite", "--full", img, "--target_storage_bytes=1400000000")

	// The boot partition starts at 8192 sectors (gokrazy's layout).
	boot := img + "@@4194304"
	config := run(t, repo, []string{"MTOOLS_SKIP_CHECK=1"}, mtype, "-i", boot, "::config.txt")
	cmdline := run(t, repo, []string{"MTOOLS_SKIP_CHECK=1"}, mtype, "-i", boot, "::cmdline.txt")
	for _, want := range []string{"enable_uart=1", "[pi4]\ndtoverlay=disable-bt", "[pi5]\ndtparam=uart0=on"} {
		if !strings.Contains(config, want) {
			t.Errorf("config.txt lacks %q:\n%s", want, config)
		}
	}
	if strings.Contains(cmdline, "serial0") || strings.Contains(cmdline, "ttyAMA0") {
		t.Errorf("cmdline.txt puts a console on the radio module's UART: %s", cmdline)
	}
	for _, f := range []string{"overlays/disable-bt.dtbo", "bcm2711-rpi-4-b.dtb", "bcm2712-rpi-5-b.dtb", "vmlinuz"} {
		if _, err := runErr(repo, []string{"MTOOLS_SKIP_CHECK=1"}, mtype, "-i", boot, "::"+f); err != nil {
			t.Errorf("boot partition lacks %s: %v", f, err)
		}
	}
}

// radioServer serves the emulated module to QEMU's chardev socket.
type radioServer struct {
	port int
	mu   sync.Mutex
	m    *fakemod.Module
}

func (r *radioServer) mod() *fakemod.Module {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		panic("QEMU has not connected to the radio module")
	}
	return r.m
}

func startRadio(t *testing.T, opts fakemod.Options) *radioServer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	r := &radioServer{port: ln.Addr().(*net.TCPAddr).Port}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			m, rw := fakemod.New(opts)
			r.mu.Lock()
			r.m = m
			r.mu.Unlock()
			go func() { io.Copy(c, rw); c.Close() }()
			go func() { io.Copy(rw, c); rw.Close() }()
		}
	}()
	return r
}

type fakeHA struct {
	c    mqtt.Client
	mu   sync.Mutex
	last map[string][]byte
}

func newFakeHA(t *testing.T, broker string) *fakeHA {
	h := &fakeHA{last: map[string][]byte{}}
	h.c = mqtt.NewClient(mqtt.NewClientOptions().AddBroker(broker).SetClientID("fake-home-assistant").SetAutoReconnect(true))
	if tok := h.c.Connect(); tok.Wait() && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	t.Cleanup(func() { h.c.Disconnect(100) })
	for _, topic := range []string{"hmbridge/#", "homeassistant/#"} {
		h.c.Subscribe(topic, 1, func(_ mqtt.Client, m mqtt.Message) {
			h.mu.Lock()
			h.last[m.Topic()] = append([]byte(nil), m.Payload()...)
			h.mu.Unlock()
		}).Wait()
	}
	return h
}

func (h *fakeHA) get(topic string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return string(h.last[topic])
}

func (h *fakeHA) json(topic string) map[string]any {
	var v map[string]any
	json.Unmarshal([]byte(h.get(topic)), &v)
	return v
}

func (h *fakeHA) forget(topic string) {
	h.mu.Lock()
	delete(h.last, topic)
	h.mu.Unlock()
}

func (h *fakeHA) publish(topic, payload string) { h.c.Publish(topic, 1, false, payload).Wait() }

func centralOf(t *testing.T, instDir string) [3]byte {
	b, err := os.ReadFile(filepath.Join(instDir, "hmbridge.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`"central_address": "([0-9A-F]{6})"`).FindSubmatch(b)
	if m == nil {
		t.Fatalf("no central_address in hmbridge.json")
	}
	var c [3]byte
	fmt.Sscanf(string(m[1]), "%02X%02X%02X", &c[0], &c[1], &c[2])
	return c
}

func editConfig(t *testing.T, instDir string, f func(map[string]any)) {
	t.Helper()
	p := filepath.Join(instDir, "config.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	f(c)
	if b, err = json.MarshalIndent(c, "", "  "); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func lookTool(t *testing.T, env, name string) string {
	t.Helper()
	if env != "" && os.Getenv(env) != "" {
		return os.Getenv(env)
	}
	p, err := exec.LookPath(name)
	if err != nil {
		for _, dir := range []string{"/usr/sbin", "/sbin"} {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				return filepath.Join(dir, name)
			}
		}
		t.Skipf("%s not found", name)
	}
	return p
}

func runErr(dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("%s %s: %v\n%s", name, strings.Join(args, " "), err, tail(out.Bytes(), 4000))
	}
	return out.String(), err
}

func run(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	out, err := runErr(dir, env, name, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func startProcess(t *testing.T, logPath, name string, args ...string) {
	t.Helper()
	logf, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(name, args...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		logf.Close()
	})
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitTCP(t *testing.T, addr string) {
	t.Helper()
	waitFor(t, 10*time.Second, "listening on "+addr, func() bool {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	start := time.Now()
	for !cond() {
		if time.Since(start) > timeout {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("%s (after %v)", what, time.Since(start).Round(100*time.Millisecond))
}

var httpClient = &http.Client{Timeout: 5 * time.Second}

func httpGetErr(url string) (string, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func httpGet(t *testing.T, url string) string {
	t.Helper()
	b, err := httpGetErr(url)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func getJSON(url string, v any) error {
	b, err := httpGetErr(url)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(b), v)
}

func tail(b []byte, n int) []byte {
	if len(b) > n {
		return b[len(b)-n:]
	}
	return b
}
