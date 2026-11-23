// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

//go:build hwtest

// Package hwtest tests a Raspberry Pi running the gokrazy image
// (contrib/gokrazy) with a real radio module, from another computer:
// over the gokrazy API, hmbridge's HTTP API and MQTT. See README.md in
// this directory for the setup and the variables.
package hwtest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

const hmbridgeService = "/user/hmbridge"

// env is the test setup, from the instance directory and HWTEST_*.
type env struct {
	instDir     string
	host        string // gokrazy host name or address
	gokPassword string
	apiURL      string // hmbridge's HTTP API
	broker      string
	mqttUser    string
	mqttPass    string
	central     string
	wantModel   string // gokrazy's model name must contain it
	wantRadio   string // module kind in /api/v1/bridge
}

var (
	setupOnce sync.Once
	theEnv    *env
	setupErr  error
)

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// lookupEnv is like getenv, but an empty value counts as set.
func lookupEnv(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

func intEnv(t *testing.T, k string) int {
	v := os.Getenv(k)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("%s=%q: %v", k, v, err)
	}
	return n
}

func setup(t *testing.T) *env {
	t.Helper()
	setupOnce.Do(func() { theEnv, setupErr = loadEnv() })
	if setupErr != nil {
		t.Skip(setupErr)
	}
	return theEnv
}

func loadEnv() (*env, error) {
	dir := os.Getenv("HWTEST_INSTANCE")
	if dir == "" {
		return nil, fmt.Errorf("HWTEST_INSTANCE is not set (the gokrazy instance directory made by new-instance.sh)")
	}
	var gcfg struct {
		Hostname string
		Update   struct {
			Hostname     string
			HTTPPassword string
			HTTPPort     string
		}
	}
	if err := readJSON(filepath.Join(dir, "config.json"), &gcfg); err != nil {
		return nil, err
	}
	var hcfg struct {
		CentralAddress string  `json:"central_address"`
		HTTPListen     *string `json:"http_listen"`
		MQTT           struct {
			Broker   string `json:"broker"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"mqtt"`
	}
	if err := readJSON(filepath.Join(dir, "hmbridge.json"), &hcfg); err != nil {
		return nil, err
	}
	host := gcfg.Hostname
	if gcfg.Update.Hostname != "" {
		host = gcfg.Update.Hostname
	}
	host = getenv("HWTEST_HOST", host)
	apiPort := "8013"
	if hcfg.HTTPListen != nil {
		if *hcfg.HTTPListen == "" {
			return nil, fmt.Errorf("hmbridge.json: http_listen is \"\"; the test needs the HTTP API")
		}
		apiPort = (*hcfg.HTTPListen)[strings.LastIndex(*hcfg.HTTPListen, ":")+1:]
	}
	gokHost := host
	if gcfg.Update.HTTPPort != "" && gcfg.Update.HTTPPort != "80" {
		gokHost += ":" + gcfg.Update.HTTPPort
	}
	return &env{
		instDir:     dir,
		host:        gokHost,
		gokPassword: gcfg.Update.HTTPPassword,
		apiURL:      getenv("HWTEST_API", "http://"+host+":"+apiPort),
		broker:      getenv("HWTEST_MQTT_BROKER", hcfg.MQTT.Broker),
		mqttUser:    hcfg.MQTT.Username,
		mqttPass:    hcfg.MQTT.Password,
		central:     strings.ToLower(hcfg.CentralAddress),
		wantModel:   lookupEnv("HWTEST_MODEL", "Raspberry Pi 3 Model B Plus"),
		wantRadio:   getenv("HWTEST_RADIO", "RPI-RF-MOD"),
	}, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

// gokrazy API (basic auth gokrazy:<Update.HTTPPassword>).

type gokIndex struct {
	Model          string
	Kernel         string
	BuildTimestamp string
	Services       []gokService
}

type gokService struct {
	Path      string
	Args      []string
	Stopped   bool
	StartTime time.Time
	Pid       int
}

func (e *env) gokRequest(method, path string) (*http.Response, error) {
	req, err := http.NewRequest(method, "http://"+e.host+path, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth("gokrazy", e.gokPassword)
	req.Header.Set("Accept", "application/json")
	return httpClient.Do(req)
}

func (e *env) index() (*gokIndex, error) {
	resp, err := e.gokRequest("GET", "/")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gokrazy: %s", resp.Status)
	}
	var idx gokIndex
	return &idx, json.NewDecoder(resp.Body).Decode(&idx)
}

func (idx *gokIndex) service(path string) *gokService {
	for i := range idx.Services {
		if idx.Services[i].Path == path {
			return &idx.Services[i]
		}
	}
	return nil
}

// logLines returns the service's recent log lines (gokrazy keeps the
// last 100 of each stream; with -debug, startup lines scroll away
// quickly).
func (e *env) logLines(path string, d time.Duration) []string {
	req, _ := http.NewRequest("GET", "http://"+e.host+"/log?path="+path+"&stream=stderr", nil)
	req.SetBasicAuth("gokrazy", e.gokPassword)
	req.Header.Set("Accept", "text/event-stream")
	c := &http.Client{Timeout: d}
	resp, err := c.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var lines []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "event:") || strings.HasPrefix(l, "id:") || strings.HasPrefix(l, ":") {
			continue
		}
		l, _ = strings.CutPrefix(l, "data: ")
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// logCollector follows a service's log from now on: lines gokrazy
// already had when it started (the previous boot's) are left out.
type logCollector struct {
	mu    sync.Mutex
	lines []string
	stop  chan struct{}
}

func (e *env) collectLog(path string) *logCollector {
	lc := &logCollector{stop: make(chan struct{})}
	old := map[string]bool{}
	for _, l := range e.logLines(path, 3*time.Second) {
		old[l] = true
	}
	go func() {
		for {
			select {
			case <-lc.stop:
				return
			default:
			}
			for _, l := range e.logLines(path, 5*time.Second) {
				lc.mu.Lock()
				if !old[l] && !slices.Contains(lc.lines, l) {
					lc.lines = append(lc.lines, l)
				}
				lc.mu.Unlock()
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()
	return lc
}

func (lc *logCollector) Stop() []string {
	close(lc.stop)
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return append([]string(nil), lc.lines...)
}

func grep(lines []string, substr string) []string {
	var out []string
	for _, l := range lines {
		if strings.Contains(l, substr) {
			out = append(out, l)
		}
	}
	return out
}

// hmbridge's HTTP API.

type bridgeInfo struct {
	Version        string `json:"version"`
	UptimeSeconds  int64  `json:"uptime_seconds"`
	CentralAddress string `json:"central_address"`
	StateDir       string `json:"state_dir"`
	Devices        int    `json:"devices"`
	Module         struct {
		Kind, Firmware, Serial, SGTIN string
	} `json:"module"`
	Pairing struct {
		Enabled    bool   `json:"enabled"`
		LastResult string `json:"last_result"`
	} `json:"pairing"`
	Warnings []string `json:"warnings"`
}

type device struct {
	Name      string         `json:"name"`
	Model     string         `json:"model"`
	Address   string         `json:"address"`
	Source    string         `json:"source"`
	Available bool           `json:"available"`
	LastSeen  *time.Time     `json:"last_seen"`
	RSSI      *int           `json:"rssi"`
	Messages  uint64         `json:"messages_received"`
	State     map[string]any `json:"state"`
	AES       struct {
		Enabled  bool `json:"enabled"`
		KeyIndex int  `json:"key_index"`
	} `json:"aes"`
}

func (e *env) apiGet(path string, v any) (http.Header, error) {
	resp, err := httpClient.Get(e.apiURL + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return resp.Header, nil
	}
	if s, ok := v.(*string); ok {
		*s = string(b)
		return resp.Header, nil
	}
	return resp.Header, json.Unmarshal(b, v)
}

func (e *env) healthy() bool {
	var h struct {
		Status string `json:"status"`
	}
	_, err := e.apiGet("/api/v1/health", &h)
	return err == nil && h.Status == "ok"
}

func (e *env) devices(t *testing.T) []device {
	t.Helper()
	var devs []device
	if _, err := e.apiGet("/api/v1/devices", &devs); err != nil {
		t.Fatalf("/api/v1/devices: %v", err)
	}
	return devs
}

func addresses(devs []device) []string {
	var a []string
	for _, d := range devs {
		a = append(a, d.Address)
	}
	slices.Sort(a)
	return a
}

// Home Assistant's side.

type fakeHA struct {
	c    mqtt.Client
	mu   sync.Mutex
	last map[string][]byte
	log  map[string][][]byte // non-retained events
}

func (e *env) ha(t *testing.T) *fakeHA {
	t.Helper()
	h := &fakeHA{last: map[string][]byte{}, log: map[string][][]byte{}}
	opts := mqtt.NewClientOptions().AddBroker(e.broker).
		SetClientID(fmt.Sprintf("hmbridge-hwtest-%d", rand.IntN(1e6))).
		SetUsername(e.mqttUser).SetPassword(e.mqttPass).SetAutoReconnect(true)
	h.c = mqtt.NewClient(opts)
	if tok := h.c.Connect(); tok.WaitTimeout(15*time.Second) && tok.Error() != nil {
		t.Fatalf("MQTT %s: %v (set HWTEST_MQTT_BROKER if the broker has another address from here)", e.broker, tok.Error())
	}
	t.Cleanup(func() { h.c.Disconnect(100) })
	for _, topic := range []string{"hmbridge/#", "homeassistant/+/hmbridge_bridge/#"} {
		h.c.Subscribe(topic, 1, func(_ mqtt.Client, m mqtt.Message) {
			h.mu.Lock()
			h.last[m.Topic()] = append([]byte(nil), m.Payload()...)
			if strings.Contains(m.Topic(), "/event/") {
				h.log[m.Topic()] = append(h.log[m.Topic()], append([]byte(nil), m.Payload()...))
			}
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

func (h *fakeHA) events(prefix string) [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out [][]byte
	for topic, evs := range h.log {
		if strings.HasPrefix(topic, prefix) {
			out = append(out, evs...)
		}
	}
	return out
}

func (h *fakeHA) publish(topic, payload string) { h.c.Publish(topic, 1, false, payload).Wait() }

// Interaction with the person at the bench (on the terminal, not
// go test's output).

type bench struct {
	in  *bufio.Reader
	out io.Writer
}

func newBench(t *testing.T) *bench {
	t.Helper()
	if os.Getenv("HWTEST_INTERACTIVE") == "" {
		t.Skip("needs someone at the devices: set HWTEST_INTERACTIVE=1")
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no terminal: %v", err)
	}
	t.Cleanup(func() { tty.Close() })
	return &bench{in: bufio.NewReader(tty), out: tty}
}

func (b *bench) ask(format string, a ...any) string {
	fmt.Fprintf(b.out, "\n>>> "+format+" ", a...)
	line, _ := b.in.ReadString('\n')
	return strings.TrimSpace(line)
}

func (b *bench) yes(format string, a ...any) bool {
	return strings.HasPrefix(strings.ToLower(b.ask(format+" [y/n]", a...)), "y")
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for !cond() {
		if time.Since(start) > timeout {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(500 * time.Millisecond)
	}
	d := time.Since(start).Round(100 * time.Millisecond)
	t.Logf("%s (after %v)", what, d)
	return d
}

func tryWait(timeout time.Duration, cond func() bool) bool {
	start := time.Now()
	for !cond() {
		if time.Since(start) > timeout {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
	return true
}

// TestBringUp checks the running system without changing anything: the
// Pi, gokrazy, the radio module, MQTT, the clock.
func TestBringUp(t *testing.T) {
	e := setup(t)
	idx, err := e.index()
	if err != nil {
		t.Fatalf("gokrazy API at %s: %v", e.host, err)
	}
	t.Logf("gokrazy: model %q, kernel %s, built %s", idx.Model, idx.Kernel, idx.BuildTimestamp)
	if !strings.Contains(idx.Model, e.wantModel) {
		t.Errorf("model %q, want %q (HWTEST_MODEL)", idx.Model, e.wantModel)
	}
	svc := idx.service(hmbridgeService)
	if svc == nil || svc.Stopped || svc.Pid == 0 {
		t.Fatalf("hmbridge is not running: %+v", svc)
	}
	if !slices.Contains(svc.Args, "-create_key_file") {
		t.Errorf("hmbridge runs without -create_key_file: %v", svc.Args)
	}
	t.Logf("hmbridge: pid %d, running since %v, args %v", svc.Pid, svc.StartTime.Local().Format(time.TimeOnly), svc.Args[1:])

	var b bridgeInfo
	if _, err := e.apiGet("/api/v1/bridge", &b); err != nil {
		t.Fatalf("hmbridge API at %s: %v", e.apiURL, err)
	}
	t.Logf("hmbridge %s, up %ds; module %s, firmware %s, serial %s, SGTIN %s; %d devices",
		b.Version, b.UptimeSeconds, b.Module.Kind, b.Module.Firmware, b.Module.Serial, b.Module.SGTIN, b.Devices)
	if b.Module.Kind != e.wantRadio {
		t.Errorf("radio module %q, want %q (HWTEST_RADIO)", b.Module.Kind, e.wantRadio)
	}
	if b.Module.Firmware == "" || b.Module.Serial == "" {
		t.Errorf("module firmware %q, serial %q: not identified", b.Module.Firmware, b.Module.Serial)
	}
	if e.wantRadio == "RPI-RF-MOD" && b.Module.SGTIN == "" {
		t.Errorf("RPI-RF-MOD without SGTIN")
	}
	if !strings.EqualFold(b.CentralAddress, e.central) {
		t.Errorf("central address %q, want %q from hmbridge.json", b.CentralAddress, e.central)
	}
	if b.StateDir != "/perm/hmbridge" || !b.Pairing.Enabled {
		t.Errorf("state dir %q, pairing enabled %v: /perm not usable?", b.StateDir, b.Pairing.Enabled)
	}
	for _, w := range b.Warnings {
		t.Logf("hmbridge warning: %s", w)
	}
	if !e.healthy() {
		t.Errorf("/api/v1/health is not ok")
	}

	var metrics string
	hdr, err := e.apiGet("/metrics", &metrics)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"hmbridge_uartgw_up 1", "hmbridge_mqtt_connected 1", "hmbridge_config_insecure_permissions 0"} {
		if !strings.Contains(metrics, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
	for _, l := range strings.Split(metrics, "\n") {
		if strings.HasPrefix(l, "hmbridge_uartgw_duty_cycle") || strings.HasPrefix(l, "hmbridge_uartgw_send_errors_total") {
			t.Logf("metric %s", l)
		}
	}

	// The Pi's clock (NTP; there is no RTC driver in the image).
	if d, err := http.ParseTime(hdr.Get("Date")); err == nil {
		skew := time.Since(d)
		t.Logf("clock: the Pi is %v behind this computer", skew.Round(time.Second))
		if skew > 10*time.Second || skew < -10*time.Second {
			t.Errorf("the Pi's clock is off by %v", skew.Round(time.Second))
		}
	}

	// The log, as far as gokrazy still has it.
	lines := e.logLines(hmbridgeService, 3*time.Second)
	for _, l := range append(grep(lines, "radio module ready"), grep(lines, "reset the radio module")...) {
		t.Logf("log: %s", l)
	}
	if e.wantRadio == "RPI-RF-MOD" {
		for _, l := range grep(lines, "reset the radio module via GPIO") {
			if !strings.Contains(l, "GPIO 19") {
				t.Errorf("RPI-RF-MOD reset on the wrong line: %s", l)
			}
		}
	}
	for _, bad := range []string{"panic:", "fatal error:"} {
		if g := grep(lines, bad); len(g) > 0 {
			t.Errorf("log: %v", g)
		}
	}

	// Home Assistant's view.
	ha := e.ha(t)
	waitFor(t, 10*time.Second, "availability online (MQTT, retained)", func() bool { return ha.get("hmbridge/bridge/availability") == "online" })
	waitFor(t, 10*time.Second, "pair button discovery", func() bool {
		return ha.json("homeassistant/button/hmbridge_bridge/pair/config")["command_topic"] == "hmbridge/bridge/set/pair"
	})

	// Is the radio receiving? Paired devices and unknown senders.
	var unk struct {
		UnknownSenders []map[string]any `json:"unknown_senders"`
		HMIPHeard      []map[string]any `json:"hmip_heard"`
	}
	e.apiGet("/api/v1/unknown", &unk)
	t.Logf("radio reception: %d unknown BidCoS senders, %d Homematic IP devices heard", len(unk.UnknownSenders), len(unk.HMIPHeard))
}

// TestPairedDevices reports every device's reception and fails for
// devices Home Assistant shows as unavailable. During the first hour
// after hmbridge's start (HWTEST_SETTLE), devices it has not heard yet
// are only reported: window contacts send on a change or about hourly.
func TestPairedDevices(t *testing.T) {
	e := setup(t)
	devs := e.devices(t)
	if len(devs) == 0 {
		t.Skip("no devices yet (TestPairing pairs some)")
	}
	var b bridgeInfo
	if _, err := e.apiGet("/api/v1/bridge", &b); err != nil {
		t.Fatal(err)
	}
	settle := time.Hour
	if v := os.Getenv("HWTEST_SETTLE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("HWTEST_SETTLE=%q: %v", v, err)
		}
		settle = d
	}
	up := time.Duration(b.UptimeSeconds) * time.Second
	started := time.Now().Add(-up)
	notYet := 0
	for _, d := range devs {
		seen, rssi := "never", "-"
		if d.LastSeen != nil {
			seen = time.Since(*d.LastSeen).Round(time.Second).String() + " ago"
		}
		if d.RSSI != nil {
			rssi = fmt.Sprintf("%d dBm", *d.RSSI)
		}
		t.Logf("%s %-20s %-18s seen %-12s rssi %-8s msgs %-5d aes %v/%d auth %v", d.Address, d.Name, d.Model, seen, rssi, d.Messages, d.AES.Enabled, d.AES.KeyIndex, d.State["auth_status"])
		heard := d.LastSeen != nil && d.LastSeen.After(started)
		switch {
		case !heard && up < settle:
			notYet++
		case !d.Available:
			t.Errorf("%s (%s) is unavailable", d.Name, d.Address)
		}
		if d.State["auth_status"] == "failed" {
			t.Errorf("%s (%s): last message failed AES authentication", d.Name, d.Address)
		}
	}
	if notYet > 0 {
		t.Logf("%d of %d devices not heard yet since hmbridge started %v ago; run again after %v (HWTEST_SETTLE) to check them", notYet, len(devs), up, settle)
	}
}

var pairedRE = regexp.MustCompile(`^paired (\S+) (\S+) \(([0-9a-f]{6})`)

// TestPairing pairs devices one by one from "Home Assistant" and
// exercises each (HWTEST_INTERACTIVE=1: someone presses the buttons).
// The devices stay paired, so that the reboot, update and power tests
// check that they survive.
func TestPairing(t *testing.T) {
	e := setup(t)
	b := newBench(t)
	ha := e.ha(t)
	for {
		if !b.yes("Pair a device?") {
			return
		}
		before := ha.json("hmbridge/bridge/pairing")["last_result"]
		ha.publish("hmbridge/bridge/set/pair", "PRESS")
		waitFor(t, 15*time.Second, "pairing mode", func() bool { return ha.json("hmbridge/bridge/pairing")["active"] == true })
		fmt.Fprintf(b.out, "\n>>> Pairing mode is on: press the device's config/pairing button now (60 s).\n")
		var m []string
		waitFor(t, 90*time.Second, "pairing result", func() bool {
			r, _ := ha.json("hmbridge/bridge/pairing")["last_result"].(string)
			if r == "" || r == before {
				return false
			}
			m = pairedRE.FindStringSubmatch(r)
			if m == nil {
				t.Logf("pairing: %s", r)
			}
			return m != nil || strings.Contains(r, "failed") || strings.Contains(r, "not supported")
		})
		if m == nil {
			t.Errorf("pairing failed: %v", ha.json("hmbridge/bridge/pairing")["last_result"])
			continue
		}
		model, addr := m[1], m[3]
		t.Logf("paired %s %s at %s", model, m[2], addr)
		t.Run(model+"_"+addr, func(t *testing.T) { exercise(t, e, b, ha, model, addr) })
	}
}

// exercise checks what a paired device does, by model.
func exercise(t *testing.T, e *env, b *bench, ha *fakeHA, model, addr string) {
	state := "hmbridge/" + addr + "/state"
	set := "hmbridge/" + addr + "/set/"
	waitFor(t, 30*time.Second, "device state published", func() bool { return ha.get(state) != "" })
	authOK := func() {
		if a := ha.json(state)["auth_status"]; a == "failed" {
			t.Errorf("AES authentication failed: auth_status %v", a)
		} else {
			t.Logf("auth_status %v", a)
		}
	}
	switch {
	case strings.HasPrefix(model, "HM-Sec-SC") || model == "HM-Sec-RHS":
		b.ask("Open the window (move the magnet away / turn the handle), then press Enter.")
		waitFor(t, 30*time.Second, "window open", func() bool { return ha.json(state)["window_open"] == true })
		authOK()
		b.ask("Close it again, then press Enter.")
		waitFor(t, 30*time.Second, "window closed", func() bool { return ha.json(state)["window_open"] == false })
		authOK()
	case model == "HM-LC-Sw1-Pl-DN-R1":
		ha.publish(set+"switch", "ON")
		waitFor(t, 30*time.Second, "switch on", func() bool { return ha.json(state)["on"] == true })
		if !b.yes("Did the plug switch on?") {
			t.Errorf("the plug did not switch on")
		}
		ha.publish(set+"switch", "OFF")
		waitFor(t, 30*time.Second, "switch off", func() bool { return ha.json(state)["on"] == false })
	case model == "HM-LC-Dim1L-Pl-3":
		ha.publish(set+"light", `{"state": "on", "brightness": 128}`)
		waitFor(t, 30*time.Second, "dimmer at about 50 %", func() bool {
			l, _ := ha.json(state)["level"].(float64)
			return l > 40 && l < 60
		})
		if !b.yes("Is the lamp at about half brightness?") {
			t.Errorf("the dimmer did not dim")
		}
		ha.publish(set+"light", `{"state": "off"}`)
		waitFor(t, 30*time.Second, "dimmer off", func() bool { return ha.json(state)["on"] == false })
	case model == "HM-CC-RT-DN" || model == "HM-TC-IT-WM-W-EU":
		// Thermostats get commands with a wake-up burst; they also
		// report on their own every few minutes.
		ha.publish(set+"temperature", "21.5")
		waitFor(t, 3*time.Minute, "target temperature 21.5", func() bool { return ha.json(state)["target_temperature"] == 21.5 })
		if !b.yes("Does the thermostat's display show 21.5?") {
			t.Errorf("the thermostat does not show the new target")
		}
		ha.publish(set+"mode", "auto")
		waitFor(t, 3*time.Minute, "mode auto", func() bool { return ha.json(state)["mode"] == "auto" })
		waitFor(t, 15*time.Minute, "current temperature reported", func() bool { _, ok := ha.json(state)["current_temperature"]; return ok })
		ha.publish(set+"settings_refresh", "PRESS")
		waitFor(t, 5*time.Minute, "settings read", func() bool {
			return ha.json("hmbridge/" + addr + "/settings")["status"] == "ok"
		})
		authOK()
	case model == "HM-PB-2-WM55" || model == "HM-RC-8":
		n := len(ha.events("hmbridge/" + addr + "/event/"))
		b.ask("Press button 1 briefly, then press Enter.")
		waitFor(t, 30*time.Second, "button event", func() bool { return len(ha.events("hmbridge/"+addr+"/event/")) > n })
		evs := ha.events("hmbridge/" + addr + "/event/")
		if !bytes.Contains(evs[len(evs)-1], []byte("press_short")) {
			t.Errorf("last event %s, want press_short", evs[len(evs)-1])
		}
	case model == "HM-OU-LED16":
		ha.publish(set+"led_1", "red")
		waitFor(t, 30*time.Second, "LED 1 red", func() bool {
			l, _ := ha.json(state)["leds"].([]any)
			return len(l) > 0 && l[0] == "red"
		})
		if !b.yes("Is LED 1 red?") {
			t.Errorf("LED 1 is not red")
		}
		ha.publish(set+"led_1", "off")
	case strings.HasPrefix(model, "HM-WDS"):
		waitFor(t, 10*time.Minute, "temperature reported", func() bool { _, ok := ha.json(state)["current_temperature"]; return ok })
	default:
		t.Logf("no checks for %s", model)
	}
}

// snapshot is what must survive reboots, updates and power cuts.
type snapshot struct {
	addrs   []string
	central string
}

func (e *env) snapshot(t *testing.T) snapshot {
	t.Helper()
	var b bridgeInfo
	if _, err := e.apiGet("/api/v1/bridge", &b); err != nil {
		t.Fatal(err)
	}
	return snapshot{addrs: addresses(e.devices(t)), central: b.CentralAddress}
}

// waitBack waits until gokrazy and hmbridge are up again after a
// reboot and checks that nothing was lost. The log collector, started
// before the reboot, catches hmbridge's startup lines.
func (e *env) waitBack(t *testing.T, before snapshot, oldStart time.Time, lc *logCollector) time.Duration {
	t.Helper()
	start := time.Now()
	waitFor(t, 5*time.Minute, "hmbridge restarted", func() bool {
		idx, err := e.index()
		if err != nil {
			return false
		}
		s := idx.service(hmbridgeService)
		return s != nil && s.Pid != 0 && s.StartTime.After(oldStart)
	})
	waitFor(t, 3*time.Minute, "hmbridge healthy", e.healthy)
	took := time.Since(start)
	after := e.snapshot(t)
	if !slices.Equal(after.addrs, before.addrs) {
		t.Errorf("devices after: %v, before: %v", after.addrs, before.addrs)
	}
	if after.central != before.central {
		t.Errorf("central address changed: %s → %s", before.central, after.central)
	}
	lines := lc.Stop()
	if g := grep(lines, "created /perm/hmbridge/system.key"); len(g) > 0 {
		t.Errorf("hmbridge created a new system key: /perm was lost: %v", g)
	}
	for _, l := range append(grep(lines, "reset the radio module"), grep(lines, "radio module ready")...) {
		t.Logf("log: %s", l)
	}
	for _, bad := range []string{"panic:", "fatal error:", "corrupt", "set aside"} {
		if g := grep(lines, bad); len(g) > 0 {
			t.Errorf("log: %v", g)
		}
	}
	return took
}

func (e *env) hmbridgeStart(t *testing.T) time.Time {
	t.Helper()
	idx, err := e.index()
	if err != nil {
		t.Fatal(err)
	}
	s := idx.service(hmbridgeService)
	if s == nil {
		t.Fatal("hmbridge is not running")
	}
	return s.StartTime
}

// TestReboot reboots the Pi HWTEST_REBOOTS times through the gokrazy
// API, without kexec: every reboot goes through the Pi's firmware and
// config.txt, like a power cycle.
func TestReboot(t *testing.T) {
	e := setup(t)
	n := intEnv(t, "HWTEST_REBOOTS")
	if n == 0 {
		t.Skip("set HWTEST_REBOOTS=<n>")
	}
	before := e.snapshot(t)
	for i := 1; i <= n; i++ {
		start := e.hmbridgeStart(t)
		lc := e.collectLog(hmbridgeService)
		resp, err := e.gokRequest("POST", "/reboot?kexec=off")
		if err != nil {
			t.Fatalf("reboot: %v", err)
		}
		resp.Body.Close()
		took := e.waitBack(t, before, start, lc)
		t.Logf("reboot %d/%d: back after %v", i, n, took.Round(time.Second))
	}
}

// TestUpdate updates the Pi HWTEST_UPDATES times with gok update (a copy
// of the instance with a new version number each time; the instance
// directory is not changed). HWTEST_TESTBOOT=1 uses gok update
// --testboot.
func TestUpdate(t *testing.T) {
	e := setup(t)
	n := intEnv(t, "HWTEST_UPDATES")
	if n == 0 {
		t.Skip("set HWTEST_UPDATES=<n>")
	}
	gok := getenv("GOK", "gok")
	if _, err := exec.LookPath(gok); err != nil {
		t.Skipf("gok: %v", err)
	}
	before := e.snapshot(t)
	parent := t.TempDir()
	name := filepath.Base(e.instDir)
	inst := filepath.Join(parent, name)
	if out, err := exec.Command("cp", "-R", e.instDir, inst).CombinedOutput(); err != nil {
		t.Fatalf("copying the instance: %v\n%s", err, out)
	}
	for i := 1; i <= n; i++ {
		version := fmt.Sprintf("hwtest-%s-%d", time.Now().Format("20060102T150405"), i)
		editConfig(t, inst, func(c map[string]any) {
			pc := c["PackageConfig"].(map[string]any)["github.com/fjaeckel/hmgo/cmd/hmbridge"].(map[string]any)
			pc["GoBuildFlags"] = []string{"-ldflags=-X github.com/fjaeckel/hmgo/internal/buildinfo.version=" + version}
			if h := os.Getenv("HWTEST_HOST"); h != "" {
				c["Update"].(map[string]any)["Hostname"] = h
			}
		})
		start := e.hmbridgeStart(t)
		lc := e.collectLog(hmbridgeService)
		args := []string{"--parent_dir", parent, "-i", name, "update"}
		if os.Getenv("HWTEST_TESTBOOT") != "" {
			args = append(args, "--testboot")
		}
		t0 := time.Now()
		if out, err := exec.Command(gok, args...).CombinedOutput(); err != nil {
			t.Fatalf("gok update: %v\n%s", err, tailBytes(out, 4000))
		}
		took := e.waitBack(t, before, start, lc)
		var b bridgeInfo
		e.apiGet("/api/v1/bridge", &b)
		if b.Version != version {
			t.Errorf("update %d: running version %q, want %q", i, b.Version, version)
		}
		t.Logf("update %d/%d: %s running %v after gok update started (%v after the reboot)", i, n, version, time.Since(t0).Round(time.Second), took.Round(time.Second))
	}
	t.Logf("the Pi now runs a test build; run gok update from your instance to go back")
}

// TestPowerCut switches the Pi's power off and on HWTEST_POWER_CYCLES
// times with a Home Assistant switch (HWTEST_POWER_SWITCH, e.g.
// switch.hmbridge_plug; HA_URL, HA_TOKEN), cutting it at a random
// moment: while booting, while hmbridge starts, or later.
func TestPowerCut(t *testing.T) {
	e := setup(t)
	n := intEnv(t, "HWTEST_POWER_CYCLES")
	sw, haURL, token := os.Getenv("HWTEST_POWER_SWITCH"), os.Getenv("HA_URL"), os.Getenv("HA_TOKEN")
	if n == 0 || sw == "" || haURL == "" || token == "" {
		t.Skip("set HWTEST_POWER_CYCLES, HWTEST_POWER_SWITCH, HA_URL and HA_TOKEN")
	}
	power := func(on bool) {
		svc := "turn_off"
		if on {
			svc = "turn_on"
		}
		req, _ := http.NewRequest("POST", strings.TrimRight(haURL, "/")+"/api/services/switch/"+svc, strings.NewReader(fmt.Sprintf(`{"entity_id": %q}`, sw)))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("Home Assistant %s: %v", svc, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("Home Assistant %s: %s", svc, resp.Status)
		}
	}
	t.Cleanup(func() { power(true) })
	before := e.snapshot(t)
	for i := 1; i <= n; i++ {
		// Up to 90 s after power on: sometimes during the boot, sometimes
		// while hmbridge sets up the module, sometimes when idle.
		cutAfter := time.Duration(rand.IntN(90)) * time.Second
		time.Sleep(cutAfter)
		power(false)
		t.Logf("cycle %d/%d: power cut %v after the previous power on", i, n, cutAfter)
		tryWait(30*time.Second, func() bool { _, err := e.index(); return err != nil })
		time.Sleep(5 * time.Second)
		power(true)
	}
	// After the last cycle: a full boot, and everything still there.
	start := time.Time{}
	lc := e.collectLog(hmbridgeService)
	took := e.waitBack(t, before, start, lc)
	t.Logf("back after the last power cut in %v", took.Round(time.Second))
}

func editConfig(t *testing.T, instDir string, f func(map[string]any)) {
	t.Helper()
	p := filepath.Join(instDir, "config.json")
	var c map[string]any
	if err := readJSON(p, &c); err != nil {
		t.Fatal(err)
	}
	f(c)
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func tailBytes(b []byte, n int) []byte {
	if len(b) > n {
		return b[len(b)-n:]
	}
	return b
}
