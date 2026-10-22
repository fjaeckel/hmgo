// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bridge

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/hmkey"
	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

// apiResponse is a recorded API response.
type apiResponse struct {
	code   int
	header http.Header
	body   []byte
}

func (r apiResponse) json(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("not a JSON object (%v):\n%s", err, r.body)
	}
	return v
}

func apiDo(h http.Handler, method, path string) apiResponse {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return apiResponse{code: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
}

// hasKeys fails unless obj has all keys (stable field names).
func hasKeys(t *testing.T, what string, obj any, keys ...string) map[string]any {
	t.Helper()
	m, ok := obj.(map[string]any)
	if !ok {
		t.Fatalf("%s: %T, want an object", what, obj)
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("%s lacks %q: %v", what, k, m)
		}
	}
	return m
}

var apiPaths = []string{"/", "/api/v1/health", "/api/v1/bridge", "/api/v1/devices", "/api/v1/devices/38f59c", "/api/v1/groups", "/api/v1/unknown", "/api/state"}

func TestAPIShapes(t *testing.T) {
	e := newEnv(t, testConfig, fakemod.Options{})
	// The radiator thermostat without AES reports (22.0 target, 22.6
	// actual, 3.0 V, valve 37 %); an unconfigured device announces
	// itself.
	e.inject(0x01, msg(0x05, 0xa6, 0x10, rtdn2, central, 0x0a, 0xb0, 0xe2, 0x0f, 0x25, 0x00))
	payload := append([]byte{0x14, 0x00, 0x95}, []byte("MEQ0123456")...)
	e.inject(0x01, msg(0x01, 0x84, 0x00, stranger, central, append(payload, 0x58, 0x00, 0x00)...))
	e.waitFor("state and unknown sender", func() bool {
		s := e.b.Snapshot()
		return len(s.Unknown) == 1 && s.Unknown[0].Serial == "MEQ0123456" && s.Devices[1].State.CurrentTemperature != nil
	})
	h := e.b.API(APIOptions{Warnings: []string{"config is readable by every user"}})

	for _, p := range apiPaths {
		r := apiDo(h, "GET", p)
		if r.code != http.StatusOK || r.header.Get("Content-Type") != "application/json" || r.header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s: %d %v", p, r.code, r.header)
		}
		if head := apiDo(h, "HEAD", p); head.code != r.code {
			t.Errorf("HEAD %s: %d", p, head.code)
		}
	}

	idx := apiDo(h, "GET", "/").json(t)
	hasKeys(t, "index", idx, "name", "version", "read_only", "note", "endpoints")
	if eps, _ := idx["endpoints"].([]any); len(eps) != len(apiEndpoints) {
		t.Errorf("endpoints %v", idx["endpoints"])
	}

	health := apiDo(h, "GET", "/api/v1/health").json(t)
	hasKeys(t, "health", health, "status", "radio", "mqtt", "clock_synced", "clock_sync_required", "warnings")
	radio := hasKeys(t, "health.radio", health["radio"], "ok", "kind", "last_error")
	mqtt := hasKeys(t, "health.mqtt", health["mqtt"], "enabled", "connected")
	if health["status"] != "ok" || radio["ok"] != true || radio["kind"] != "HM-MOD-RPI-PCB" || mqtt["enabled"] != false || health["clock_synced"] != true {
		t.Errorf("health %v", health)
	}
	if w, _ := health["warnings"].([]any); len(w) != 1 || w[0] != "config is readable by every user" {
		t.Errorf("health warnings %v", health["warnings"])
	}

	br := apiDo(h, "GET", "/api/v1/bridge").json(t)
	hasKeys(t, "bridge", br, "version", "go_version", "started", "uptime_seconds", "module", "central_address",
		"state_dir", "pairing", "devices", "command_queue", "warnings")
	mod := hasKeys(t, "bridge.module", br["module"], "kind", "firmware", "serial", "sgtin")
	pairing := hasKeys(t, "bridge.pairing", br["pairing"], "enabled", "disabled_reason", "active", "status",
		"remaining_seconds", "aes", "last_result", "last_result_at", "unpair_waiting")
	if br["central_address"] != "1A2B3C" || br["devices"] != 4.0 || mod["kind"] != "HM-MOD-RPI-PCB" || mod["firmware"] == "" ||
		!strings.HasPrefix(br["go_version"].(string), "go") || pairing["active"] != false || pairing["status"] != "idle" || pairing["last_result_at"] != nil {
		t.Errorf("bridge %s", apiDo(h, "GET", "/api/v1/bridge").body)
	}

	var devs []map[string]any
	if err := json.Unmarshal(apiDo(h, "GET", "/api/v1/devices").body, &devs); err != nil || len(devs) != 4 {
		t.Fatalf("devices: %v %v", err, devs)
	}
	for _, d := range devs {
		hasKeys(t, "device", d, "name", "model", "address", "serial", "source", "group", "available", "last_seen", "rssi",
			"messages_received", "aes", "last_event", "summary", "last_command", "state", "settings")
		hasKeys(t, "device.aes", d["aes"], "enabled", "channels", "key_index")
	}
	one := apiDo(h, "GET", "/api/v1/devices/38F5A1").json(t)
	state := one["state"].(map[string]any)
	if one["name"] != "Wohnzimmer Heizung 2" || one["source"] != "config" || one["group"] != "Wohnzimmer" || one["available"] != true ||
		one["last_seen"] == nil || one["messages_received"] != 1.0 || one["aes"].(map[string]any)["enabled"] != false ||
		state["current_temperature"] != 22.6 || state["valve_position"] != 37.0 {
		t.Errorf("device %s", apiDo(h, "GET", "/api/v1/devices/38f5a1").body)
	}
	hasKeys(t, "device.settings", one["settings"], "current", "status")
	aes := apiDo(h, "GET", "/api/v1/devices/38f59c").json(t)["aes"].(map[string]any)
	if chs, _ := aes["channels"].([]any); aes["enabled"] != true || aes["key_index"] != 1.0 || len(chs) != 6 || chs[0] != 1.0 {
		t.Errorf("aes %v", aes)
	}
	if contact := apiDo(h, "GET", "/api/v1/devices/4c5d6e").json(t); contact["last_seen"] != nil || contact["aes"].(map[string]any)["enabled"] != false {
		t.Errorf("contact %v", contact)
	}

	groups := apiDo(h, "GET", "/api/v1/groups").json(t)
	if gs, _ := groups["groups"].([]any); len(gs) != 1 {
		t.Errorf("groups %v", groups)
	} else {
		hasKeys(t, "group", gs[0], "name", "slug", "members", "state")
	}

	unk := apiDo(h, "GET", "/api/v1/unknown").json(t)
	hasKeys(t, "unknown", unk, "unknown_senders", "hmip_heard")
	us, _ := unk["unknown_senders"].([]any)
	if len(us) != 1 {
		t.Fatalf("unknown %v", unk)
	}
	u := hasKeys(t, "unknown sender", us[0], "address", "messages", "last_seen", "last_cmd", "rssi", "serial", "model_id")
	if u["serial"] != "MEQ0123456" || u["address"] != "112233" {
		t.Errorf("unknown sender %v", u)
	}

	legacy := apiDo(h, "GET", "/api/state")
	if legacy.header.Get("Deprecation") != "true" || !bytes.Contains(legacy.body, []byte(`"MEQ0123456"`)) || !bytes.Contains(legacy.body, []byte(`"warnings"`)) {
		t.Errorf("/api/state %v\n%s", legacy.header, legacy.body)
	}
}

func TestAPINotFoundAndMethods(t *testing.T) {
	e := newEnv(t, testConfig, fakemod.Options{})
	h := e.b.API(APIOptions{})
	for _, p := range []string{"/api/v1/devices/abcdef", "/api/v1/devices/38f59", "/api/v1/devices/zz", "/api/v1/devices/", "/api/v1/nope", "/index.html", "/pairing/start"} {
		r := apiDo(h, "GET", p)
		if r.code != http.StatusNotFound || r.header.Get("Content-Type") != "application/json" {
			t.Errorf("GET %s: %d %v", p, r.code, r.header)
		}
		if msg, _ := r.json(t)["error"].(string); msg == "" {
			t.Errorf("GET %s: no error message", p)
		}
	}
	for _, p := range append(apiPaths, "/pairing/start", "/nope") {
		for _, m := range []string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
			r := apiDo(h, m, p)
			if r.code != http.StatusMethodNotAllowed || r.header.Get("Allow") != "GET, HEAD" || r.header.Get("Content-Type") != "application/json" {
				t.Errorf("%s %s: %d %v", m, p, r.code, r.header)
			}
		}
	}
	// Nothing a request can do starts pairing (it is an MQTT command).
	time.Sleep(50 * time.Millisecond)
	if p := e.b.Snapshot().Pairing; p.Status != "idle" || len(e.b.commands) != 0 {
		t.Errorf("a refused request reached the bridge: pairing %+v", p)
	}
}

// connPub is a publisher that knows whether it is connected (like
// ha.Client).
type connPub struct {
	fakePub
	connected atomic.Bool
}

func (p *connPub) Connected() bool { return p.connected.Load() }

func TestAPIHealthDegradedAndDown(t *testing.T) {
	cfg := strings.Replace(testConfig, `"mqtt": {"broker": ""}`, `"mqtt": {"broker": "tcp://127.0.0.1:1"}`, 1)
	var synced atomic.Bool
	e := newEnvWith(t, cfg, fakemod.Options{}, func(o *Options) { o.ClockSynced = synced.Load })
	pub := &connPub{}
	e.b.SetPublisher(pub)
	h := e.b.API(APIOptions{})
	health := func() (int, map[string]any) {
		r := apiDo(h, "GET", "/api/v1/health")
		return r.code, r.json(t)
	}

	code, v := health()
	if code != http.StatusOK || v["status"] != "degraded" || v["clock_synced"] != false || v["mqtt"].(map[string]any)["connected"] != false {
		t.Errorf("%d %v", code, v)
	}
	if w := v["warnings"].([]any); len(w) != 2 {
		t.Errorf("warnings %v", w)
	}
	synced.Store(true)
	pub.connected.Store(true)
	if code, v = health(); code != http.StatusOK || v["status"] != "ok" || len(v["warnings"].([]any)) != 0 {
		t.Errorf("%d %v", code, v)
	}

	// The radio module fails: down, 503.
	if err := e.mod.Crash(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-e.run:
		e.run <- err // for the cleanup
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after module reset")
	}
	code, v = health()
	radio := v["radio"].(map[string]any)
	if code != http.StatusServiceUnavailable || v["status"] != "down" || radio["ok"] != false || radio["last_error"] == "" {
		t.Errorf("%d %v", code, v)
	}
	if r := apiDo(h, "HEAD", "/api/v1/health"); r.code != http.StatusServiceUnavailable {
		t.Errorf("HEAD: %d", r.code)
	}
}

// No response contains key material: neither the system key (also
// given to AES devices on pairing) nor the eQ-3 default key, in any
// encoding.
func TestAPINoKeyMaterial(t *testing.T) {
	e := newEnv(t, testConfig, fakemod.Options{})
	e.inject(0x01, msg(0x05, 0xa6, 0x10, rtdn2, central, 0x0a, 0xb0, 0xe2, 0x0f, 0x25, 0x00))
	e.waitFor("state", func() bool { return e.b.Snapshot().Devices[1].State.CurrentTemperature != nil })
	cfg, err := config.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := cfg.Keys()
	var needles [][]byte
	for _, k := range [][16]byte{cur.Bytes, hmkey.EQ3Default.Bytes} {
		for _, s := range []string{
			hex.EncodeToString(k[:]), strings.ToUpper(hex.EncodeToString(k[:])),
			base64.StdEncoding.EncodeToString(k[:]), base64.RawStdEncoding.EncodeToString(k[:]),
			base64.URLEncoding.EncodeToString(k[:]), base64.RawURLEncoding.EncodeToString(k[:]),
		} {
			needles = append(needles, []byte(s))
		}
		needles = append(needles, k[:], k[:8], k[8:])
		// A JSON array of the bytes ([]int).
		ints, _ := json.Marshal(func() []int {
			r := make([]int, 16)
			for i, b := range k {
				r[i] = int(b)
			}
			return r
		}())
		needles = append(needles, bytes.Trim(ints, "[]"))
	}
	needles = append(needles, []byte(`"secret"`)) // the passphrase
	h := e.b.API(APIOptions{})
	for _, p := range append(apiPaths, "/api/v1/devices/38f5a1", "/api/v1/devices/abcdef", "/nope") {
		for _, m := range []string{"GET", "POST"} {
			r := apiDo(h, m, p)
			for _, n := range needles {
				if bytes.Contains(r.body, n) {
					t.Errorf("%s %s contains key material %q", m, p, n)
				}
			}
		}
	}
}
