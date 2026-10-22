// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bridge

// The read-only JSON API (docs/DEPLOY.md, “HTTP API”). It only reports;
// every action (pairing, unpairing, renaming, device settings) is done
// from Home Assistant over MQTT. No response ever contains key
// material: devices are described by their AES channels and key index.
//
// Locking: each handler copies what it needs under b.mu (the same
// copies Snapshot makes) and encodes the copy after releasing it, so
// that a slow client never holds up the event loop.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/fjaeckel/hmgo/internal/ha"
)

// maxAPIResponse bounds an API response. The data is bounded anyway
// (configured and paired devices, at most maxUnknownSenders and
// maxHMIPDevices); this is the last line of defence.
const maxAPIResponse = 4 << 20

// APIOptions configure the HTTP API.
type APIOptions struct {
	// Warnings are configuration problems (e.g. an insecure
	// configuration file), reported by /api/v1/health and
	// /api/v1/bridge.
	Warnings []string
}

// Health status values of /api/v1/health.
const (
	HealthOK       = "ok"
	HealthDegraded = "degraded"
	HealthDown     = "down"
)

// APIHealth is the response of GET /api/v1/health.
type APIHealth struct {
	// Status is "ok", "degraded" (MQTT not connected, or the required
	// clock synchronisation missing) or "down" (radio module failed).
	Status string         `json:"status"`
	Radio  APIRadioHealth `json:"radio"`
	MQTT   APIMQTTHealth  `json:"mqtt"`
	// ClockSynced reports whether the system clock is NTP-synchronised;
	// ClockSyncRequired whether that is needed (require_synced_clock).
	ClockSynced       bool `json:"clock_synced"`
	ClockSyncRequired bool `json:"clock_sync_required"`
	// Warnings are the reasons for "degraded" and configuration
	// problems (which alone do not degrade the status).
	Warnings []string `json:"warnings"`
}

// APIRadioHealth is the radio module part of APIHealth.
type APIRadioHealth struct {
	OK        bool   `json:"ok"`
	Kind      string `json:"kind"`
	LastError string `json:"last_error"`
}

// APIMQTTHealth is the MQTT part of APIHealth.
type APIMQTTHealth struct {
	// Enabled is false with an empty mqtt.broker.
	Enabled   bool `json:"enabled"`
	Connected bool `json:"connected"`
}

// APIBridge is the response of GET /api/v1/bridge.
type APIBridge struct {
	Version        string     `json:"version"`
	GoVersion      string     `json:"go_version"`
	Started        time.Time  `json:"started"`
	UptimeSeconds  int64      `json:"uptime_seconds"`
	Module         APIModule  `json:"module"`
	CentralAddress string     `json:"central_address"`
	StateDir       string     `json:"state_dir"`
	Pairing        APIPairing `json:"pairing"`
	Devices        int        `json:"devices"`
	CommandQueue   int        `json:"command_queue"`
	Warnings       []string   `json:"warnings"`
}

// APIModule describes the radio module.
type APIModule struct {
	Kind     string `json:"kind"`
	Firmware string `json:"firmware"`
	Serial   string `json:"serial"`
	// SGTIN is only known for the RPI-RF-MOD.
	SGTIN string `json:"sgtin"`
}

// APIPairing is the pairing state in APIBridge.
type APIPairing struct {
	Enabled bool `json:"enabled"`
	// DisabledReason says why pairing is disabled ("" if enabled).
	DisabledReason   string     `json:"disabled_reason"`
	Active           bool       `json:"active"`
	Status           string     `json:"status"`
	RemainingSeconds int        `json:"remaining_seconds"`
	AES              string     `json:"aes"`
	LastResult       string     `json:"last_result"`
	LastResultAt     *time.Time `json:"last_result_at"`
	// UnpairWaiting lists devices whose config button must be pressed
	// to complete their unpairing.
	UnpairWaiting []string `json:"unpair_waiting"`
}

// APIDevice is an element of GET /api/v1/devices and the response of
// GET /api/v1/devices/{address}.
type APIDevice struct {
	Name    string `json:"name"`
	Model   string `json:"model"`
	Address string `json:"address"`
	Serial  string `json:"serial"`
	// Source is "config" (configuration file) or "paired" (paired with
	// hmbridge, devices.json).
	Source    string     `json:"source"`
	Group     string     `json:"group"`
	Available bool       `json:"available"`
	LastSeen  *time.Time `json:"last_seen"`
	RSSI      *int       `json:"rssi"`
	Messages  uint64     `json:"messages_received"`
	AES       APIAES     `json:"aes"`
	// LastEvent is the type of the last decoded message; Summary a
	// short rendering of an actuator's or remote's state.
	LastEvent   string `json:"last_event"`
	Summary     string `json:"summary"`
	LastCommand string `json:"last_command"`
	// State is the device state as published to Home Assistant.
	State ha.State `json:"state"`
	// Settings is the settings cache as published to Home Assistant
	// (current values, status, per-setting detail, read_at); null for
	// models without settings.
	Settings *ha.SettingsState `json:"settings"`
}

// APIAES describes a device's AES configuration (never the key).
type APIAES struct {
	Enabled  bool  `json:"enabled"`
	Channels []int `json:"channels"`
	KeyIndex int   `json:"key_index"`
}

// APIUnknown is the response of GET /api/v1/unknown.
type APIUnknown struct {
	UnknownSenders []UnknownSender `json:"unknown_senders"`
	HMIPHeard      []HMIPDevice    `json:"hmip_heard"`
}

// APIGroups is the response of GET /api/v1/groups.
type APIGroups struct {
	Groups []GroupSnapshot `json:"groups"`
}

// APIIndex is the response of GET /.
type APIIndex struct {
	Name      string        `json:"name"`
	Version   string        `json:"version"`
	ReadOnly  bool          `json:"read_only"`
	Note      string        `json:"note"`
	Endpoints []APIEndpoint `json:"endpoints"`
}

// APIEndpoint describes an endpoint in APIIndex.
type APIEndpoint struct {
	Path        string `json:"path"`
	Description string `json:"description"`
}

// APIError is the body of error responses.
type APIError struct {
	Error string `json:"error"`
}

var apiEndpoints = []APIEndpoint{
	{"/api/v1/health", "overall status, radio module, MQTT, clock; HTTP 503 when down"},
	{"/api/v1/bridge", "version, uptime, radio module, central address, pairing state, state directory, warnings"},
	{"/api/v1/devices", "all devices with state, AES configuration and settings"},
	{"/api/v1/devices/{address}", "one device by its radio address (6 hex digits)"},
	{"/api/v1/groups", "heating groups"},
	{"/api/v1/unknown", "unconfigured BidCoS senders and Homematic IP devices heard"},
	{"/metrics", "Prometheus metrics"},
}

var apiAddrRE = regexp.MustCompile(`^[0-9a-f]{6}$`)

type api struct {
	b        *Bridge
	warnings []string
}

// API returns the read-only JSON API (GET and HEAD only, see ReadOnly):
//
//	/                          index of the endpoints
//	/api/v1/health             APIHealth (503 when down)
//	/api/v1/bridge             APIBridge
//	/api/v1/devices            []APIDevice
//	/api/v1/devices/{address}  APIDevice (404 if unknown)
//	/api/v1/groups             APIGroups
//	/api/v1/unknown            APIUnknown
//	/api/state                 deprecated: the former status page JSON
//
// Mount it behind HostAllowlist (DNS rebinding).
func (b *Bridge) API(o APIOptions) http.Handler {
	a := &api{b: b, warnings: append([]string{}, o.Warnings...)}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", a.health)
	mux.HandleFunc("/api/v1/bridge", a.bridge)
	mux.HandleFunc("/api/v1/devices", a.devices)
	mux.HandleFunc("/api/v1/devices/{address}", a.device)
	mux.HandleFunc("/api/v1/groups", a.groups)
	mux.HandleFunc("/api/v1/unknown", a.unknown)
	mux.HandleFunc("/api/state", a.legacyState)
	mux.HandleFunc("/{$}", a.index)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, APIError{Error: "not found; GET / lists the endpoints"})
	})
	return ReadOnly(mux)
}

// writeJSON encodes v (outside any lock) and sends it with code.
func writeJSON(w http.ResponseWriter, code int, v any) {
	body, err := json.MarshalIndent(v, "", "  ")
	if err == nil && len(body) > maxAPIResponse {
		err = fmt.Errorf("response too large (%d bytes)", len(body))
	}
	if err != nil {
		code = http.StatusInternalServerError
		body, _ = json.Marshal(APIError{Error: "encoding the response: " + err.Error()})
	}
	body = append(body, '\n')
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(code)
	w.Write(body)
}

func (a *api) index(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, APIIndex{
		Name:      "hmbridge",
		Version:   a.b.version,
		ReadOnly:  true,
		Note:      "read-only; pairing, unpairing, renaming and device settings are done in Home Assistant (MQTT)",
		Endpoints: apiEndpoints,
	})
}

// radioModule returns what the radio module reports about itself.
func (b *Bridge) radioModule() APIModule {
	var m APIModule
	if r, ok := b.radio.(interface{ Kind() string }); ok {
		m.Kind = r.Kind()
	}
	if r, ok := b.radio.(interface{ FirmwareVersion() string }); ok {
		m.Firmware = r.FirmwareVersion()
	}
	if r, ok := b.radio.(interface{ SerialNumber() string }); ok {
		m.Serial = r.SerialNumber()
	}
	if r, ok := b.radio.(interface{ SGTIN() string }); ok {
		m.SGTIN = r.SGTIN()
	}
	return m
}

// Health returns the health report of /api/v1/health. configWarnings
// are added to its warnings.
func (b *Bridge) Health(configWarnings []string) APIHealth {
	h := APIHealth{Status: HealthOK, Warnings: []string{}}
	h.Radio.Kind = b.radioModule().Kind
	if err := b.radio.Err(); err != nil {
		h.Radio.LastError = err.Error()
	} else {
		h.Radio.OK = true
	}

	b.mu.Lock()
	pub := b.pub
	pairWarnings := append([]string(nil), b.pair.warnings...)
	b.mu.Unlock()
	h.MQTT.Enabled = b.cfg.MQTT.BrokerURL() != ""
	if pub != nil {
		h.MQTT.Connected = true
		// ha.Client knows whether it is connected right now (it
		// reconnects by itself); other publishers (tests) are.
		if c, ok := pub.(interface{ Connected() bool }); ok {
			h.MQTT.Connected = c.Connected()
		}
	}
	h.ClockSyncRequired = b.cfg.RequiresSyncedClock()
	h.ClockSynced = b.clockSync()

	degraded := false
	if !h.Radio.OK {
		h.Status = HealthDown
		h.Warnings = append(h.Warnings, "radio module failed: "+h.Radio.LastError)
	}
	if h.MQTT.Enabled && !h.MQTT.Connected {
		degraded = true
		h.Warnings = append(h.Warnings, "not connected to the MQTT broker")
	}
	if h.ClockSyncRequired && !h.ClockSynced {
		degraded = true
		h.Warnings = append(h.Warnings, "system clock not synchronised (NTP): device time requests are not answered")
	}
	if degraded && h.Status == HealthOK {
		h.Status = HealthDegraded
	}
	h.Warnings = append(h.Warnings, configWarnings...)
	h.Warnings = append(h.Warnings, pairWarnings...)
	return h
}

func (a *api) health(w http.ResponseWriter, r *http.Request) {
	h := a.b.Health(a.warnings)
	code := http.StatusOK
	if h.Status == HealthDown {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, h)
}

func (a *api) bridge(w http.ResponseWriter, r *http.Request) {
	b := a.b
	res := APIBridge{
		Version:        b.version,
		GoVersion:      runtime.Version(),
		Module:         b.radioModule(),
		CentralAddress: strings.ToUpper(fmt.Sprintf("%x", b.cfg.CentralAddr())),
		StateDir:       b.store.Dir(),
		Warnings:       append([]string{}, a.warnings...),
	}
	b.mu.Lock()
	now := b.now()
	res.Started = b.started
	ps := b.pairingSnapshotLocked(now)
	active := b.pairingStateLocked(now).Active
	res.Devices = len(b.devices)
	res.CommandQueue = b.queue.len()
	b.mu.Unlock()

	if !res.Started.IsZero() {
		res.UptimeSeconds = int64(now.Sub(res.Started) / time.Second)
	}
	res.Pairing = APIPairing{
		Enabled: ps.Enabled, DisabledReason: ps.DisabledReason, Active: active, Status: ps.Status,
		RemainingSeconds: ps.Remaining, AES: ps.AES, LastResult: ps.LastResult,
		UnpairWaiting: append([]string{}, ps.UnpairWaiting...),
	}
	if !ps.LastResultAt.IsZero() {
		t := ps.LastResultAt
		res.Pairing.LastResultAt = &t
	}
	res.Warnings = append(res.Warnings, ps.Warnings...)
	writeJSON(w, http.StatusOK, res)
}

// apiDeviceLocked copies d for the API. Caller holds b.mu.
func (b *Bridge) apiDeviceLocked(d *device) APIDevice {
	var st ha.State
	st.Merge(d.state)
	res := APIDevice{
		Name:        d.cfg.Name,
		Model:       d.cfg.Model,
		Address:     d.desc.Addr,
		Serial:      d.cfg.Serial,
		Source:      "config",
		Available:   d.available,
		RSSI:        st.RSSI,
		Messages:    d.msgs,
		LastEvent:   d.lastEvent,
		Summary:     summary(d.desc.Kind, &st, d.lastEvent),
		LastCommand: st.LastCommand,
		State:       st,
		AES:         APIAES{Enabled: d.cfg.AESEnabled(), Channels: []int{}, KeyIndex: int(d.cfg.KeyIndex())},
	}
	if d.stored != nil {
		res.Source = "paired"
	}
	if d.group != nil {
		res.Group = d.group.name
	}
	if !st.LastSeen.IsZero() {
		t := st.LastSeen
		res.LastSeen = &t
	}
	if res.AES.Enabled {
		for _, ch := range d.cfg.AESChannelList() {
			res.AES.Channels = append(res.AES.Channels, int(ch))
		}
	}
	if d.settings != nil {
		s := b.settingsStateLocked(d)
		res.Settings = &s
	}
	return res
}

func (a *api) devices(w http.ResponseWriter, r *http.Request) {
	b := a.b
	b.mu.Lock()
	res := make([]APIDevice, 0, len(b.devices))
	for _, d := range b.devices {
		res = append(res, b.apiDeviceLocked(d))
	}
	b.mu.Unlock()
	writeJSON(w, http.StatusOK, res)
}

func (a *api) device(w http.ResponseWriter, r *http.Request) {
	addr := strings.ToLower(r.PathValue("address"))
	if !apiAddrRE.MatchString(addr) {
		writeJSON(w, http.StatusNotFound, APIError{Error: "no such device; the address has 6 hex digits"})
		return
	}
	b := a.b
	b.mu.Lock()
	d := b.deviceByAddrHex(addr)
	var res APIDevice
	if d != nil {
		res = b.apiDeviceLocked(d)
	}
	b.mu.Unlock()
	if d == nil {
		writeJSON(w, http.StatusNotFound, APIError{Error: "no such device: " + addr})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *api) groups(w http.ResponseWriter, r *http.Request) {
	s := a.b.Snapshot()
	res := APIGroups{Groups: s.Groups}
	if res.Groups == nil {
		res.Groups = []GroupSnapshot{}
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *api) unknown(w http.ResponseWriter, r *http.Request) {
	b := a.b
	res := APIUnknown{UnknownSenders: []UnknownSender{}, HMIPHeard: []HMIPDevice{}}
	b.mu.Lock()
	for _, u := range b.unknown {
		res.UnknownSenders = append(res.UnknownSenders, *u)
	}
	b.mu.Unlock()
	sort.Slice(res.UnknownSenders, func(i, j int) bool {
		return res.UnknownSenders[i].LastSeen.After(res.UnknownSenders[j].LastSeen)
	})
	if b.hmip != nil {
		res.HMIPHeard = append(res.HMIPHeard, b.hmip.Heard()...)
	}
	writeJSON(w, http.StatusOK, res)
}

// legacyState is the deprecated /api/state: the JSON of the former
// status page. To be removed in the release after the one introducing
// /api/v1.
func (a *api) legacyState(w http.ResponseWriter, r *http.Request) {
	m := a.b.radioModule()
	v := struct {
		Snapshot
		Info     map[string]string `json:"info,omitempty"`
		Warnings []string          `json:"warnings,omitempty"`
	}{
		Snapshot: a.b.Snapshot(),
		Info:     map[string]string{"version": a.b.version, "module": m.Kind, "firmware": m.Firmware, "serial": m.Serial},
		Warnings: a.warnings,
	}
	w.Header().Set("Deprecation", "true")
	w.Header().Set("Link", `</api/v1/devices>; rel="successor-version"`)
	writeJSON(w, http.StatusOK, v)
}
