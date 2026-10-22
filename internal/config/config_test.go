package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fjaeckel/hmgo/internal/hmkey"
)

const minimal = `{"central_address":"1A2B3C","devices":[{"name":"a","model":"HM-CC-RT-DN","address":"38F59C"}]}`

func TestExampleParses(t *testing.T) {
	cfg, err := Load("example.json")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(cfg.Devices), 5; got != want {
		t.Fatalf("devices: got %d, want %d", got, want)
	}
	cur, prev := cfg.Keys()
	if cur.Index != 2 || prev == nil || prev.Index != 1 {
		t.Fatalf("keys: got %v, %v", cur, prev)
	}
	gs := cfg.ResolvedGroups()
	if len(gs) != 1 {
		t.Fatalf("groups: got %d", len(gs))
	}
	g := gs[0]
	var names []string
	for _, d := range g.Thermostats {
		names = append(names, d.Name)
	}
	if want := []string{"Wohnzimmer Wandthermostat", "Wohnzimmer Heizung", "Wohnzimmer Heizung Fenster"}; !reflect.DeepEqual(names, want) {
		t.Errorf("thermostat order: got %q, want %q", names, want)
	}
	if g.Climate.Model != ModelTC {
		t.Errorf("climate source: got %s", g.Climate.Model)
	}
	if len(g.Contacts) != 1 || g.Contacts[0].Model != ModelSCo {
		t.Errorf("contacts: %+v", g.Contacts)
	}
	if got := cfg.GroupOf([3]byte{0x38, 0xf5, 0xa1}); got != "Wohnzimmer" {
		t.Errorf("GroupOf: got %q", got)
	}
	if got := cfg.GroupOf([3]byte{0x38, 0xf5, 0xb2}); got != "" {
		t.Errorf("GroupOf(ungrouped): got %q", got)
	}
}

func TestDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SerialPort != "/dev/serial0" || cfg.Reset != "gpio" || cfg.ResetGPIO != 18 {
		t.Errorf("serial defaults: %q %q %d", cfg.SerialPort, cfg.Reset, cfg.ResetGPIO)
	}
	if got := cfg.MQTT.BrokerURL(); got != "tcp://localhost:1883" {
		t.Errorf("broker: %q", got)
	}
	if cfg.MQTT.ClientID != "hmbridge" || cfg.MQTT.BaseTopic != "hmbridge" || cfg.MQTT.DiscoveryPrefix != "homeassistant" {
		t.Errorf("mqtt defaults: %+v", cfg.MQTT)
	}
	if got := cfg.HTTPListenAddr(); got != ":8013" {
		t.Errorf("http_listen: %q", got)
	}
	if got := cfg.TimeSyncInterval.Duration; got != time.Hour {
		t.Errorf("time_sync_interval: %v", got)
	}
	if got, want := cfg.CentralAddr(), [3]byte{0x1a, 0x2b, 0x3c}; got != want {
		t.Errorf("central: %x", got)
	}
	cur, prev := cfg.Keys()
	if *cur != hmkey.EQ3Default || prev != nil {
		t.Errorf("keys: %v %v", cur, prev)
	}
	d := cfg.Devices[0]
	if d.Addr() != [3]byte{0x38, 0xf5, 0x9c} || d.AddrHex() != "38f59c" {
		t.Errorf("addr: %x %s", d.Addr(), d.AddrHex())
	}
	if d.AESChannelList() != nil {
		t.Errorf("aes channels without aes: %v", d.AESChannelList())
	}
	if d.Channels() != 6 {
		t.Errorf("channels: %d", d.Channels())
	}
}

func TestDisable(t *testing.T) {
	cfg, err := Parse([]byte(`{"central_address":"0x1a2b3c","mqtt":{"broker":""},"http_listen":"","time_sync_interval":"30m",
		"devices":[{"name":"a","model":"HM-TC-IT-WM-W-EU","address":"0x2a1b3c","aes":true}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MQTT.BrokerURL() != "" || cfg.HTTPListenAddr() != "" {
		t.Errorf("not disabled: %q %q", cfg.MQTT.BrokerURL(), cfg.HTTPListenAddr())
	}
	if cfg.TimeSyncInterval.Duration != 30*time.Minute {
		t.Errorf("interval: %v", cfg.TimeSyncInterval)
	}
	if got, want := cfg.Devices[0].AESChannelList(), []byte{1, 2, 3, 4, 5, 6, 7}; !reflect.DeepEqual(got, want) {
		t.Errorf("default aes channels: got %v, want %v", got, want)
	}
}

func TestKeys(t *testing.T) {
	for _, tt := range []struct {
		name     string
		security string
		cur      string // hex
		curIdx   byte
		prev     string
		prevIdx  byte
	}{
		{
			name:     "passphrase default index",
			security: `{"passphrase":"abc"}`,
			cur:      "900150983cd24fb0d6963f7d28e17f72", // md5("abc")
			curIdx:   1,
		},
		{
			name:     "hex key",
			security: `{"key":"00112233445566778899AABBCCDDEEFF","index":5}`,
			cur:      "00112233445566778899aabbccddeeff",
			curIdx:   5,
		},
		{
			name:     "previous passphrase defaults to index-1",
			security: `{"key":"00112233445566778899aabbccddeeff","index":3,"previous":{"passphrase":"abc"}}`,
			cur:      "00112233445566778899aabbccddeeff",
			curIdx:   3,
			prev:     "900150983cd24fb0d6963f7d28e17f72",
			prevIdx:  2,
		},
		{
			name:     "previous explicit index",
			security: `{"passphrase":"abc","index":4,"previous":{"key":"ffeeddccbbaa99887766554433221100","index":1}}`,
			cur:      "900150983cd24fb0d6963f7d28e17f72",
			curIdx:   4,
			prev:     "ffeeddccbbaa99887766554433221100",
			prevIdx:  1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(`{"central_address":"1A2B3C","security":` + tt.security + `,"devices":[{"name":"a","model":"HM-CC-RT-DN","address":"38F59C"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			cur, prev := cfg.Keys()
			want, _ := hmkey.FromHex(tt.cur, tt.curIdx)
			if *cur != want {
				t.Errorf("current: got %x/%d, want %s/%d", cur.Bytes, cur.Index, tt.cur, tt.curIdx)
			}
			if tt.prev == "" {
				if prev != nil {
					t.Errorf("unexpected previous key")
				}
				return
			}
			wantPrev, _ := hmkey.FromHex(tt.prev, tt.prevIdx)
			if prev == nil || *prev != wantPrev {
				t.Errorf("previous: got %+v, want %s/%d", prev, tt.prev, tt.prevIdx)
			}
		})
	}
}

func TestValidationErrors(t *testing.T) {
	dev := `{"name":"a","model":"HM-CC-RT-DN","address":"38F59C"}`
	wrap := func(top, devices string) string {
		return `{"central_address":"1A2B3C"` + top + `,"devices":[` + devices + `]}`
	}
	const secretKey = "00112233445566778899aabbccddeeff"
	for _, tt := range []struct {
		name, json, want string
	}{
		{"syntax", `{"central_address":`, "invalid"},
		{"unknown field", wrap(`,"serialport":"x"`, dev), `unknown field "serialport"`},
		{"unknown device field", wrap(``, `{"name":"a","model":"HM-CC-RT-DN","address":"38F59C","adress":"x"}`), `unknown field "adress"`},
		{"wrong type", wrap(`,"reset_gpio":"18"`, dev), "reset_gpio"},
		{"trailing data", minimal + `{}`, "after the top-level object"},
		{"missing central", `{"devices":[` + dev + `]}`, "central_address is required"},
		{"bad central", `{"central_address":"1A2B","devices":[` + dev + `]}`, "6 hex digits"},
		{"non-hex central", `{"central_address":"1A2B3G","devices":[` + dev + `]}`, "not hexadecimal"},
		{"zero central", `{"central_address":"000000","devices":[` + dev + `]}`, "must not be 000000"},
		{"bad reset", wrap(`,"reset":"usb"`, dev), "reset"},
		{"no devices", wrap(``, ``), "at least one device"},
		{"bad model", wrap(``, `{"name":"a","model":"HM-CC-TC","address":"38F59C"}`), "unsupported model"},
		{"missing name", wrap(``, `{"model":"HM-CC-RT-DN","address":"38F59C"}`), "name is required"},
		{"missing address", wrap(``, `{"name":"a","model":"HM-CC-RT-DN"}`), "address is required"},
		{"bad address", wrap(``, `{"name":"a","model":"HM-CC-RT-DN","address":"38F59"}`), "6 hex digits"},
		{"address = central", wrap(``, `{"name":"a","model":"HM-CC-RT-DN","address":"1a2b3c"}`), "equals central_address"},
		{"dup name", wrap(``, dev+`,{"name":"A","model":"HM-CC-RT-DN","address":"38F59D"}`), "duplicate name"},
		{"dup address", wrap(``, dev+`,{"name":"b","model":"HM-CC-RT-DN","address":"0x38f59c"}`), "duplicate address"},
		{"bad serial", wrap(``, `{"name":"a","model":"HM-CC-RT-DN","address":"38F59C","serial":"MEQ"}`), "serial"},
		{"bad aes channel", wrap(``, `{"name":"a","model":"HM-Sec-SCo","address":"38F59C","aes_channels":[2]}`), "out of range"},
		{"aes channel 0", wrap(``, `{"name":"a","model":"HM-CC-RT-DN","address":"38F59C","aes_channels":[0,4]}`), "maintenance channel"},
		{"unknown key index", wrap(`,"security":{"passphrase":"x","index":2}`, `{"name":"a","model":"HM-CC-RT-DN","address":"38F59C","key_index":1}`), "key_index 1 is not the index of a configured key; must be 2 (current)"},
		{"key and passphrase", wrap(`,"security":{"key":"`+secretKey+`","passphrase":"topsecret"}`, dev), "not both"},
		{"empty passphrase", wrap(`,"security":{"passphrase":""}`, dev), "must not be empty"},
		{"empty security", wrap(`,"security":{}`, dev), "key or passphrase is required"},
		{"short key", wrap(`,"security":{"key":"00112233"}`, dev), "32 hex"},
		{"non-hex key", wrap(`,"security":{"key":"zz112233445566778899aabbccddeeff"}`, dev), "32 hex"},
		{"index range", wrap(`,"security":{"passphrase":"topsecret","index":256}`, dev), "out of range"},
		{"negative index", wrap(`,"security":{"passphrase":"topsecret","index":-1}`, dev), "out of range"},
		{"previous same index", wrap(`,"security":{"passphrase":"topsecret","index":1,"previous":{"passphrase":"old","index":1}}`, dev), "equals the current"},
		{"previous index 0 needs explicit", wrap(`,"security":{"passphrase":"topsecret","index":0,"previous":{"passphrase":"old"}}`, dev), "index is required"},
		{"bad duration", wrap(`,"time_sync_interval":"soon"`, dev), `invalid duration "soon"`},
		{"short stale_after", wrap(``, `{"name":"a","model":"HM-CC-RT-DN","address":"38F59C","stale_after":"10s"}`), "stale_after 10s is too short"},
		{"duration not string", wrap(`,"time_sync_interval":3600`, dev), "duration"},
		{"short duration", wrap(`,"time_sync_interval":"1s"`, dev), "too short"},
		{"wildcard topic", wrap(`,"mqtt":{"base_topic":"a/#"}`, dev), "wildcards"},
		{"group unknown member", wrap(`,"groups":[{"name":"g","members":["nope"]}]`, dev), `member "nope"`},
		{"group no thermostat", wrap(`,"groups":[{"name":"g","members":["w"]}]`, dev+`,{"name":"w","model":"HM-Sec-SCo","address":"4C5D6E"}`), "at least one thermostat"},
		{"device in two groups", wrap(`,"groups":[{"name":"g","members":["a"]},{"name":"h","members":["38f59c"]}]`, dev), "already a member"},
		{"dup group", wrap(`,"groups":[{"name":"g","members":["a"]},{"name":"G","members":["b"]}]`, dev+`,{"name":"b","model":"HM-CC-RT-DN","address":"38F59D"}`), "duplicate group"},
		{"group no name", wrap(`,"groups":[{"members":["a"]}]`, dev), "name is required"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.json))
			if err == nil {
				t.Fatalf("Parse succeeded, want error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
			for _, secret := range []string{secretKey, "topsecret", "00112233"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error leaks key material: %q", err)
				}
			}
		})
	}
}

func TestAESChannelsExplicit(t *testing.T) {
	cfg, err := Parse([]byte(`{"central_address":"1A2B3C","devices":[{"name":"a","model":"HM-CC-RT-DN","address":"38F59C","aes_channels":[4,1]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Devices[0]
	if !d.AESEnabled() {
		t.Error("aes_channels should imply AES")
	}
	if got, want := d.AESChannelList(), []byte{4, 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestLoadError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(`{"central_address":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), p) {
		t.Fatalf("want error mentioning path, got %v", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("want error for missing file")
	}
}

func TestKeyIndexAndOptions(t *testing.T) {
	c, err := Parse([]byte(`{"central_address":"1A2B3C",
	  "security":{"passphrase":"new","index":3,"previous":{"passphrase":"old","index":2}},
	  "devices":[
	    {"name":"a","model":"HM-CC-RT-DN","address":"38F59C"},
	    {"name":"b","model":"HM-CC-RT-DN","address":"38F59D","key_index":2},
	    {"name":"w","model":"HM-TC-IT-WM-W-EU","address":"38F59E"}],
	  "groups":[{"name":"g","via_wall_thermostat":true,"members":["a","b","w"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Devices[0].KeyIndex(); got != 3 {
		t.Errorf("default key index = %d, want 3", got)
	}
	if got := c.Devices[1].KeyIndex(); got != 2 {
		t.Errorf("key_index = %d, want 2", got)
	}
	if !c.AnswersTimeRequests() || c.CSMACA {
		t.Errorf("defaults: answer_time_requests=%v csma_ca=%v, want true/false", c.AnswersTimeRequests(), c.CSMACA)
	}
	rg := c.ResolvedGroups()[0]
	if len(rg.CommandTargets) != 1 || rg.CommandTargets[0].Name != "w" {
		t.Errorf("command targets = %v, want only the wall thermostat", rg.CommandTargets)
	}
	if len(rg.Thermostats) != 3 {
		t.Errorf("thermostats = %d, want 3", len(rg.Thermostats))
	}
}
