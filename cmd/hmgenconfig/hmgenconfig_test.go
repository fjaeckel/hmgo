// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/fjaeckel/hmgo/internal/config"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func servers(t *testing.T) (xmlrpcURL, regaURL string) {
	t.Helper()
	listDevices := fixture(t, "listDevices.xml")
	rega := fixture(t, "rega.txt")
	x := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || !bytes.Contains(b, []byte("<methodName>listDevices</methodName>")) {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		w.Write(listDevices)
	}))
	t.Cleanup(x.Close)
	rg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || !bytes.Contains(b, []byte("ID_DEVICES")) {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Write(rega)
	}))
	t.Cleanup(rg.Close)
	return x.URL + "/", rg.URL + "/rega.exe"
}

func generate(t *testing.T, src Sources) (*config.Config, map[string]any, string) {
	t.Helper()
	var log, dump bytes.Buffer
	src.Log = &log
	if src.Dump == nil {
		src.Dump = &dump
	}
	out, err := Generate(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	// The placeholders must be filled in: hmbridge rejects them.
	if bytes.Contains(out, []byte(placeholder)) {
		if _, err := config.Parse(out); err == nil || !strings.Contains(err.Error(), placeholder) {
			t.Errorf("unfilled config: Parse error %v, want a %s placeholder error", err, placeholder)
		}
	}
	cfg, err := config.Parse(fillPlaceholders(out))
	if err != nil {
		t.Fatalf("generated config does not validate: %v\n%s\nlog:\n%s", err, out, log.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	return cfg, raw, string(out) + "\n--- log:\n" + log.String()
}

// fillPlaceholders does what the user does: replace the placeholder
// passphrases and MQTT credentials with real values.
func fillPlaceholders(out []byte) []byte {
	out = bytes.ReplaceAll(out, []byte(`"`+placeholder+`-PREVIOUS"`), []byte(`"old passphrase"`))
	return bytes.ReplaceAll(out, []byte(`"`+placeholder+`"`), []byte(`"filled in"`))
}

func device(cfg *config.Config, name string) *config.Device {
	for i := range cfg.Devices {
		if cfg.Devices[i].Name == name {
			return &cfg.Devices[i]
		}
	}
	return nil
}

func TestGenerateAllSources(t *testing.T) {
	xu, ru := servers(t)
	var dump bytes.Buffer
	cfg, raw, out := generate(t, Sources{
		RfdDir:     "testdata/rfd",
		IDsPath:    "testdata/ids",
		HMModePath: "testdata/hm_mode",
		XMLRPCURL:  xu,
		ReGaURL:    ru,
		GroupsPath: "testdata/groups.gson",
		Dump:       &dump,
	})

	if got := cfg.CentralAddress; got != "458086" {
		t.Errorf("central_address = %q", got)
	}
	if cfg.Radio != config.RadioRPIRFMod || cfg.SerialPort != "/dev/raw-uart" || cfg.Reset != "none" {
		t.Errorf("radio/serial/reset = %q %q %q", cfg.Radio, cfg.SerialPort, cfg.Reset)
	}
	if cfg.StateFilePath() != "" {
		t.Errorf("state_file = %q", cfg.StateFilePath())
	}
	if cur, prev := cfg.Keys(); cur.Index != 2 || prev == nil || prev.Index != 1 {
		t.Errorf("key indexes: current %v previous %v", cur, prev)
	}
	if len(cfg.Devices) != 19 {
		t.Errorf("%d devices, want 19\n%s", len(cfg.Devices), out)
	}

	type want struct {
		name, serial string
		aes          bool
		channels     []byte
		keyIndex     byte
	}
	for _, w := range []want{
		{"Büro Heizung", "NEQ0873206", true, []byte{1, 2, 3, 4, 5, 6}, 2},
		{"Büro Wandthermostat", "MEQ1571441", true, []byte{2}, 2},
		{"Büro Fenster", "SEQ2094299", true, []byte{1}, 2},
		{"Gästezimmer Tür", "SEQ2094449", true, []byte{1}, 2},
		{"Küche Heizung", "PEQ1315854", true, []byte{1, 2, 3, 4, 5, 6}, 2},
		{"Esszimmer Heizkörper NEQ1007152", "NEQ1007152", true, []byte{1, 2, 3, 4, 5, 6}, 2},
		{"Esszimmer Heizkörper PEQ1190989", "PEQ1190989", false, nil, 2},
		{"Room A HM-CC-RT-DN", "NEQ1009870", true, []byte{1, 2, 3, 4, 5, 6}, 1},
		// The other classic devices of the user's installation.
		{"Stehlampe Schalter", "LEQ0123456", false, nil, 2},
		{"Stehlampe Esszimmer", "PEQ1107611", true, []byte{1}, 2},
		{"Leselampe", "MEQ1116747", false, nil, 2},
		{"Taster Diele", "REQ1215000", true, []byte{1, 2}, 2},
		{"Fernbedienung", "PEQ1918407", true, []byte{1, 2, 3, 4, 5, 6, 7}, 2},
		{"Statusanzeige", "SEQ1234485", false, nil, 2},
		{"Außen Nord", "QEQ1859460", false, nil, 2},
		{"Wassertemperatur", "TEQ0694924", false, nil, 2},
	} {
		d := device(cfg, w.name)
		if d == nil {
			t.Errorf("device %q missing\n%s", w.name, out)
			continue
		}
		if d.Serial != w.serial || d.AESEnabled() != w.aes || !reflect.DeepEqual(d.AESChannelList(), w.channels) || d.KeyIndex() != w.keyIndex {
			t.Errorf("device %q: serial %s aes %v channels %v key index %d", w.name, d.Serial, d.AESEnabled(), d.AESChannelList(), d.KeyIndex())
		}
	}
	if d := device(cfg, "Büro Heizung"); d != nil && d.AddrHex() != "3a1001" {
		t.Errorf("Büro Heizung address %s", d.AddrHex())
	}
	// aes_channels is only written when not all channels are secured.
	for _, dv := range raw["devices"].([]any) {
		m := dv.(map[string]any)
		switch m["name"] {
		case "Büro Heizung", "Taster Diele":
			if m["aes_channels"] != nil {
				t.Errorf("aes_channels written for the fully secured %v", m)
			}
		case "Fernbedienung":
			if m["aes_channels"] == nil {
				t.Errorf("aes_channels missing for the partly secured %v", m)
			}
		}
	}

	groups := cfg.ResolvedGroups()
	if len(groups) != 3 {
		t.Fatalf("%d groups, want 3\n%s", len(groups), out)
	}
	names := []string{groups[0].Name, groups[1].Name, groups[2].Name}
	if !reflect.DeepEqual(names, []string{"Room A", "Room B", "Room C"}) {
		t.Errorf("groups = %v", names)
	}
	if g := groups[0]; len(g.Thermostats) != 4 || len(g.Contacts) != 0 || g.Climate.Model != config.ModelTC {
		t.Errorf("Room A: %d thermostats, %d contacts", len(g.Thermostats), len(g.Contacts))
	}
	if g := groups[1]; len(g.Thermostats) != 2 || len(g.Contacts) != 1 || g.Contacts[0].Name != "Büro Fenster" {
		t.Errorf("Room B: %+v", g)
	}
	// Devices are sorted by group, then name; ungrouped last.
	if cfg.Devices[0].Name != "Esszimmer Heizkörper NEQ1007152" || cfg.Devices[len(cfg.Devices)-1].Name != "Wassertemperatur" {
		t.Errorf("device order: first %q last %q", cfg.Devices[0].Name, cfg.Devices[len(cfg.Devices)-1].Name)
	}

	comments, _ := raw["_comment"].([]any)
	var all []string
	for _, c := range comments {
		all = append(all, c.(string))
	}
	joined := strings.Join(all, "\n")
	for _, s := range []string{
		"not supported yet: Bewegungsmelder Diele (HM-Sen-MDIR-WM55 1D2000)",
		"not a device: Zentrale (HM-RCV-50 458086)",
	} {
		if !strings.Contains(joined, s) {
			t.Errorf("_comment lacks %q:\n%s", s, joined)
		}
	}
	if strings.Contains(joined, "Stehlampe") || strings.Contains(joined, "LED16") {
		t.Errorf("_comment lists supported devices:\n%s", joined)
	}
	// Models without AES get neither aes nor an "AES state unknown" note.
	for _, name := range []string{"Außen Nord", "Wassertemperatur"} {
		for _, dv := range raw["devices"].([]any) {
			m := dv.(map[string]any)
			if m["name"] == name && (m["aes"] != nil || m["_comment"] != nil) {
				t.Errorf("%s: %v", name, m)
			}
		}
	}
	sec := raw["security"].(map[string]any)
	if sec["passphrase"] != placeholder {
		t.Errorf("security = %v", sec)
	}
	if !strings.Contains(dump.String(), "NEQ0873206:2") || !strings.Contains(dump.String(), "Büro Heizung") || !strings.Contains(dump.String(), "HomeMatic.heating") {
		t.Errorf("dump incomplete:\n%s", dump.String())
	}
}

func TestGenerateFilesOnly(t *testing.T) {
	// OpenCCU not running: XML-RPC and ReGa unreachable.
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	cfg, _, out := generate(t, Sources{
		RfdDir:     "testdata/rfd",
		IDsPath:    "testdata/ids",
		XMLRPCURL:  url + "/",
		ReGaURL:    url + "/rega.exe",
		GroupsPath: "testdata/groups.gson",
	})
	if len(cfg.Devices) != 11 || len(cfg.Groups) != 3 {
		t.Errorf("%d devices, %d groups\n%s", len(cfg.Devices), len(cfg.Groups), out)
	}
	if d := device(cfg, "Room B HM-CC-RT-DN"); d == nil || d.AESEnabled() {
		t.Errorf("Room B HM-CC-RT-DN: %+v\n%s", d, out)
	}
	if d := device(cfg, "HM-CC-RT-DN PEQ1315854"); d == nil {
		t.Errorf("fallback name missing\n%s", out)
	}
	if !strings.Contains(out, "AES state unknown") || !strings.Contains(out, "is OpenCCU running?") {
		t.Errorf("missing notes:\n%s", out)
	}
	if cfg.Radio != config.RadioAuto {
		t.Errorf("radio = %q", cfg.Radio)
	}
}

func TestGenerateXMLRPCOnly(t *testing.T) {
	// No files at all: devices and the central address come from rfd.
	xu, _ := servers(t)
	cfg, _, out := generate(t, Sources{XMLRPCURL: xu})
	if cfg.CentralAddress != "458086" || len(cfg.Devices) != 19 || cfg.Security != nil {
		t.Errorf("central %q, %d devices, security %v\n%s", cfg.CentralAddress, len(cfg.Devices), cfg.Security, out)
	}
}
