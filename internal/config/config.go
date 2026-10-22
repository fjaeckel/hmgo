// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

// Package config loads and validates the hmbridge JSON configuration.
//
// See example.json in this directory for a documented example. All
// fields except central_address and devices are optional; defaults are
// documented on the respective struct fields.
//
// Error messages never contain key material (key or passphrase).
package config

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fjaeckel/hmgo/internal/hmkey"
)

// Supported device models.
const (
	ModelRTDN = "HM-CC-RT-DN"      // radiator thermostat (valve drive)
	ModelTC   = "HM-TC-IT-WM-W-EU" // wall thermostat
	ModelSC2  = "HM-Sec-SC-2"      // shutter contact (magnetic)
	ModelSC   = "HM-Sec-SC"        // shutter contact (magnetic, 1st gen)
	ModelSCo  = "HM-Sec-SCo"       // shutter contact (optical)
	ModelRHS  = "HM-Sec-RHS"       // rotary handle sensor (closed/tilted/open)

	ModelSwitch = "HM-LC-Sw1-Pl-DN-R1" // switch plug
	ModelDimmer = "HM-LC-Dim1L-Pl-3"   // dimmer plug
	ModelPB2    = "HM-PB-2-WM55"       // 2-button wall switch
	ModelRC8    = "HM-RC-8"            // 8-button remote
	ModelLED16  = "HM-OU-LED16"        // 16-LED status display
	ModelWDSTH  = "HM-WDS10-TH-O"      // outdoor temperature/humidity sensor
	ModelWDST   = "HM-WDS30-T-O"       // outdoor temperature sensor
)

// modelChannels maps each supported model to its highest channel
// number (channel 0 is the maintenance channel and always exists).
// Taken from the channel lists in eQ-3's rftypes XML
// (rf_cc_rt_dn.xml, rf_tc_it_wm-w-eu.xml, rf_sc.xml, rf_sec_sco.xml,
// rf_rhs.xml, rf_s_644.xml, rf_dim_1l_644.xml, rf_pb-2-wm55*.xml,
// rf_rc-4-2.xml, rf_ou_led16*.xml, rf_ash550.xml, rf_s550ia.xml).
var modelChannels = map[string]int{
	ModelRTDN: 6, // 1 weather rx, 2 climate rx, 3 window rx, 4 climate (control), 5 RT team, 6 remote
	ModelTC:   7, // 1 weather tx, 2 thermal control (control), 3 window rx, 6 remote, 7 switch tx
	ModelSC2:  1,
	ModelSC:   1,
	ModelSCo:  1,
	ModelRHS:  1,

	ModelSwitch: 1,
	ModelDimmer: 3, // 1 dimmer (physical), 2 and 3 virtual dimmer channels
	ModelPB2:    2, // one KEY channel per button
	ModelRC8:    8,
	ModelLED16:  16, // one KEY channel per LED
	ModelWDSTH:  1,
	ModelWDST:   1,
}

// noAES lists the models without AES support (the weather sensors'
// rftypes descriptions have no AES_ACTIVE parameter).
var noAES = map[string]bool{
	ModelWDSTH: true,
	ModelWDST:  true,
}

// Models returns the supported model names.
func Models() []string {
	return []string{ModelRTDN, ModelTC, ModelSC2, ModelSC, ModelSCo, ModelRHS,
		ModelSwitch, ModelDimmer, ModelPB2, ModelRC8, ModelLED16, ModelWDSTH, ModelWDST}
}

// SupportsAES reports whether model can use AES.
func SupportsAES(model string) bool { return modelChannels[model] > 0 && !noAES[model] }

// IsActuator reports whether model is a mains powered actuator that
// hmbridge sends commands to and polls for its status (switch, dimmer,
// LED display).
func IsActuator(model string) bool {
	return model == ModelSwitch || model == ModelDimmer || model == ModelLED16
}

// IsRemote reports whether model is a push button or remote control.
func IsRemote(model string) bool { return model == ModelPB2 || model == ModelRC8 }

// IsWeather reports whether model is an outdoor weather sensor.
func IsWeather(model string) bool { return model == ModelWDSTH || model == ModelWDST }

// ModelChannels returns the highest channel number of model, or 0 if
// the model is unknown.
func ModelChannels(model string) int { return modelChannels[model] }

// IsThermostat reports whether model is a thermostat (RT-DN or TC).
func IsThermostat(model string) bool { return model == ModelRTDN || model == ModelTC }

// IsContact reports whether model is a window/door contact.
func IsContact(model string) bool {
	switch model {
	case ModelSC2, ModelSC, ModelSCo, ModelRHS:
		return true
	}
	return false
}

// Radio modules.
const (
	RadioAuto        = "auto"
	RadioHMModRPIPCB = "hm-mod-rpi-pcb"
	RadioRPIRFMod    = "rpi-rf-mod"
)

// Defaults.
const (
	DefaultSerialPort       = "/dev/serial0"
	DefaultReset            = "auto"
	DefaultResetGPIO        = 18 // HM-MOD-RPI-PCB
	ResetGPIORPIRFMod       = 19
	DefaultBroker           = "tcp://localhost:1883"
	DefaultClientID         = "hmbridge"
	DefaultBaseTopic        = "hmbridge"
	DefaultDiscoveryPrefix  = "homeassistant"
	DefaultHTTPListen       = ":8013"
	DefaultTimeSyncInterval = time.Hour
)

// Config is the parsed and validated configuration.
type Config struct {
	Comment    Comment `json:"_comment,omitempty"`
	SerialPort string  `json:"serial_port"` // default /dev/serial0
	// Reset selects how the radio module is reset at startup: "gpio"
	// (the module's reset line on reset_gpio), "raw-uart" (ask
	// OpenCCU's raw-uart kernel driver, which knows the line of either
	// module), "none", or "auto" (default: "raw-uart" for /dev/raw-uart*,
	// else "gpio", except for an RPI-RF-MOD that is not on the Pi's own
	// UART, e.g. on a USB adapter: "none").
	Reset string `json:"reset"`
	// Radio selects the radio module: "auto" (default, detected at
	// startup), "hm-mod-rpi-pcb" or "rpi-rf-mod".
	Radio string `json:"radio,omitempty"`
	// ResetGPIO is the BCM GPIO number of the reset line. 0 or absent
	// means the module's line: 18 for the HM-MOD-RPI-PCB, 19 for the
	// RPI-RF-MOD; with "radio": "auto" it is chosen at startup (see
	// ResetGPIOAuto).
	ResetGPIO int `json:"reset_gpio"`

	// CentralAddress is the BidCoS address of the CCU to impersonate
	// (6 hex digits, optional 0x prefix). Required.
	CentralAddress string `json:"central_address"`

	// Security is optional; absent means the eQ-3 default key (index 0).
	Security *Security `json:"security,omitempty"`

	MQTT MQTT `json:"mqtt"`

	// HTTPListen is the listen address of the read-only JSON API
	// (/api/v1/…) and /metrics. Default ":8013"; "" disables the HTTP
	// server.
	HTTPListen *string `json:"http_listen,omitempty"`

	// HTTPAllowedHosts lists additional host names under which the
	// API and /metrics may be reached (the Host header of the
	// request), e.g. a DNS name or a reverse proxy's name. IP addresses,
	// "localhost" and the machine's host name (also with ".local") are
	// always accepted; requests for other names are refused, so that a
	// web site cannot reach hmbridge through DNS rebinding. "*" accepts
	// every name (disables that protection).
	HTTPAllowedHosts []string `json:"http_allowed_hosts,omitempty"`

	// TimeSyncInterval is how often the radio module's clock is set
	// (Go duration string). Default "1h".
	TimeSyncInterval Duration `json:"time_sync_interval"`

	// CSMACA enables listen-before-talk on the radio module. Default
	// false, like the CCU, FHEM and Homegear.
	CSMACA bool `json:"csma_ca,omitempty"`

	// AnswerTimeRequests makes hmbridge answer the devices' time
	// requests itself. Default true. The radio module may already answer
	// them from its own clock (which hmbridge keeps in sync); set false
	// if the debug log shows duplicate answers.
	AnswerTimeRequests *bool `json:"answer_time_requests,omitempty"`

	// RequireSyncedClock makes hmbridge answer time requests and set the
	// radio module's clock only while the kernel reports the system
	// clock as synchronised (NTP). A Raspberry Pi has no RTC: after a
	// power cut it may run with a stale clock until NTP is reachable,
	// and the devices would shift their week programs. Default true;
	// set false on systems without NTP whose clock is set otherwise.
	RequireSyncedClock *bool `json:"require_synced_clock,omitempty"`

	// StateDir is the directory where hmbridge keeps its data (see
	// package store): state.json, the last known device state, so that
	// a restart does not blank Home Assistant until every device has
	// reported again when the broker's retained messages are not
	// available; and devices.json, the devices paired with hmbridge.
	// Default "/var/lib/hmbridge"; must be absolute; "" disables
	// persistence (and pairing).
	StateDir *string `json:"state_dir,omitempty"`

	// StateFile is deprecated (use StateDir): an explicit path of the
	// state file, "" disables only the state file.
	StateFile *string `json:"state_file,omitempty"`

	// StateSaveInterval is the minimum time between two writes of the
	// state file (Go duration, default "15m"). The file is only written
	// when something significant changed (mode, target, window, battery,
	// fault), and once on shutdown; normal temperature reports never
	// cause a write. The broker's retained messages are the primary
	// source after a restart, so this only matters if the broker is
	// unreachable at startup.
	StateSaveIntervalSetting Duration `json:"state_save_interval,omitempty"`

	// StatusPollIntervalSetting is how often the mains powered actuators
	// (switch, dimmer, LED display) are asked for their status (Go
	// duration, default "1h", "0s" disables polling). They report only
	// when something changes, so the poll is what tells whether they are
	// still reachable (their default stale_after is three intervals).
	// They are always asked once shortly after startup.
	StatusPollIntervalSetting Duration `json:"status_poll_interval,omitempty"`

	// DevicesFile is deprecated (use StateDir): an explicit path of the
	// file with the devices paired with hmbridge itself (see Pairing),
	// "" disables pairing. The devices file is read at startup and
	// merged with Devices (on a conflict, Devices wins) and only written
	// when a device is paired, unpaired or renamed.
	DevicesFile *string `json:"devices_file,omitempty"`

	// Pairing configures the teach-in of new devices.
	Pairing *Pairing `json:"pairing,omitempty"`

	// Devices are the devices defined in this file. Devices paired with
	// hmbridge are kept in devices.json in StateDir instead.
	Devices []Device `json:"devices"`
	Groups  []Group  `json:"groups,omitempty"`
	// GroupsSuggestArea sends each heating group's name as the Home
	// Assistant suggested_area of the group's device and of its members'
	// devices. Default true. Home Assistant applies a suggested area only
	// when it creates a device (first discovery), never to an existing
	// one, so this neither moves devices later nor overrides areas set in
	// Home Assistant.
	GroupsSuggestArea *bool `json:"groups_suggest_area,omitempty"`

	centralAddr [3]byte
	current     hmkey.Key
	previous    *hmkey.Key
	// resetGPIOAuto: reset_gpio was not set and the radio is detected
	// at startup.
	resetGPIOAuto bool
	// permWarnings are problems with the configuration file's
	// permissions found by Load.
	permWarnings []string
	// keyWarnings are problems with the key files' permissions found
	// while resolving security.key_file.
	keyWarnings []string
}

// Pairing configures how hmbridge pairs new devices.
type Pairing struct {
	Comment Comment `json:"_comment,omitempty"`
	// AES selects what pairing does about AES: "system_key" switches
	// AES-capable devices to the configured key (security) and enables
	// AES on the channels a CCU enables it on (contacts, push buttons,
	// remotes); "off" leaves the devices on the eQ-3 default key without
	// AES. Default: "system_key" when a security block is configured,
	// else "off".
	AES string `json:"aes,omitempty"`
	// Duration is how long pairing mode stays on (Go duration, default
	// "60s", 10s … 10m).
	Duration Duration `json:"duration,omitempty"`
}

// Pairing AES modes.
const (
	PairingAESSystemKey = "system_key"
	PairingAESOff       = "off"
)

// DefaultStateDir is where hmbridge keeps its data.
const DefaultStateDir = "/var/lib/hmbridge"

// File names in the state directory.
const (
	StateFileName   = "state.json"
	DevicesFileName = "devices.json"
)

// DefaultPairingDuration is how long pairing mode stays on.
const DefaultPairingDuration = 60 * time.Second

// Security configures the AES key used by the devices (the CCU's
// “system security key”). Exactly one of Key, Passphrase and KeyFile
// may be set.
type Security struct {
	Comment    Comment `json:"_comment,omitempty"`
	Key        string  `json:"key,omitempty"`        // 32 hex chars
	Passphrase *string `json:"passphrase,omitempty"` // CCU passphrase; key = MD5(passphrase)
	// KeyFile is the absolute path of a file holding the key as 32 hex
	// characters (written by hmbridge init, mode 0600), so that the
	// configuration file does not contain it.
	KeyFile  string   `json:"key_file,omitempty"`
	Index    *int     `json:"index,omitempty"`    // key index, default 1 when a key is set
	Previous *KeySpec `json:"previous,omitempty"` // optional older key
}

// KeySpec is a key given as hex, as passphrase or as key file.
type KeySpec struct {
	Comment    Comment `json:"_comment,omitempty"`
	Key        string  `json:"key,omitempty"`
	Passphrase *string `json:"passphrase,omitempty"`
	KeyFile    string  `json:"key_file,omitempty"`
	Index      *int    `json:"index,omitempty"`
}

// MQTT configures the Home Assistant connection.
type MQTT struct {
	Comment Comment `json:"_comment,omitempty"`
	// Broker URL. Default tcp://localhost:1883; "" disables MQTT.
	Broker          *string `json:"broker,omitempty"`
	Username        string  `json:"username,omitempty"`
	Password        string  `json:"password,omitempty"`
	ClientID        string  `json:"client_id,omitempty"`        // default "hmbridge"
	BaseTopic       string  `json:"base_topic,omitempty"`       // default "hmbridge"
	DiscoveryPrefix string  `json:"discovery_prefix,omitempty"` // default "homeassistant"
}

// BrokerURL returns the resolved broker URL ("" = MQTT disabled).
func (m MQTT) BrokerURL() string {
	if m.Broker == nil {
		return DefaultBroker
	}
	return *m.Broker
}

// Device is one BidCoS device.
type Device struct {
	Comment Comment `json:"_comment,omitempty"`
	Name    string  `json:"name"`
	Model   string  `json:"model"`
	Address string  `json:"address"`          // 6 hex digits
	Serial  string  `json:"serial,omitempty"` // optional, 10 chars (e.g. "MEQ0123456")
	AES     bool    `json:"aes,omitempty"`
	// AESChannels optionally lists the channels on which the radio
	// module enforces AES. If AES is true and the list is empty, all
	// functional channels 1..ModelChannels(model) are used (channel 0,
	// maintenance, is excluded). Setting AESChannels implies AES.
	AESChannels []int `json:"aes_channels,omitempty"`
	// KeyIndexOverride is the AES key index this device uses, if it
	// differs from security.index (e.g. a device that was not re-keyed
	// after the CCU's key change still uses security.previous.index).
	// Must be the index of the current or previous key.
	KeyIndexOverride *int `json:"key_index,omitempty"`
	// StaleAfter is how long the device may stay silent before Home
	// Assistant shows it as unavailable (Go duration, "0s" = never).
	// Default: 30m for thermostats (they report every few minutes), 1h
	// for the weather sensors, 6h for the HM-Sec-SCo, 50h for the other
	// contacts (they report about daily), three status poll intervals for
	// the actuators, and never for remotes (they only send when a button
	// is pressed).
	StaleAfter Duration `json:"stale_after,omitempty"`

	addr         [3]byte
	keyIndex     byte
	staleDefault time.Duration
	fromStore    bool
}

// FromStore reports whether the device comes from the devices file
// (paired with hmbridge) rather than from the configuration file.
func (d Device) FromStore() bool { return d.fromStore }

// KeyIndex returns the AES key index the device uses: key_index if set,
// otherwise the index of the current key.
func (d Device) KeyIndex() byte { return d.keyIndex }

// Addr returns the parsed BidCoS address.
func (d Device) Addr() [3]byte { return d.addr }

// AddrHex returns the address as 6 lowercase hex digits.
func (d Device) AddrHex() string { return hex.EncodeToString(d.addr[:]) }

// Channels returns the highest channel number of the device's model.
func (d Device) Channels() int { return modelChannels[d.Model] }

// AESEnabled reports whether AES is enforced on any channel.
func (d Device) AESEnabled() bool { return d.AES || len(d.AESChannels) > 0 }

// AESChannelList returns the channels on which AES is enforced (nil
// when AES is disabled).
func (d Device) AESChannelList() []byte {
	if !d.AESEnabled() {
		return nil
	}
	if len(d.AESChannels) > 0 {
		res := make([]byte, len(d.AESChannels))
		for i, c := range d.AESChannels {
			res[i] = byte(c)
		}
		return res
	}
	n := d.Channels()
	res := make([]byte, 0, n)
	for c := 1; c <= n; c++ {
		res = append(res, byte(c))
	}
	return res
}

// HasAESChannel reports whether AES is enforced on channel ch.
func (d Device) HasAESChannel(ch byte) bool {
	for _, c := range d.AESChannelList() {
		if c == ch {
			return true
		}
	}
	return false
}

// Group is a CCU-style heating group: thermostats (RT-DN and/or TC)
// plus optional window contacts. Members are given by device name or
// address.
type Group struct {
	Comment Comment  `json:"_comment,omitempty"`
	Name    string   `json:"name"`
	Members []string `json:"members"`
	// ViaWallThermostat sends group commands only to the group's wall
	// thermostat(s), which forward them to their peered radiator
	// thermostats. This halves the radio airtime of group commands but
	// requires the direct TC↔RT-DN links the CCU sets up for a group.
	// Default false: every thermostat gets the command.
	ViaWallThermostat bool `json:"via_wall_thermostat,omitempty"`

	thermostats []int // indexes into Config.Devices, TC first
	contacts    []int
}

// ResolvedGroup is a Group with its members resolved to devices.
type ResolvedGroup struct {
	Name string
	// Thermostats in command order: wall thermostats (TC) first, then
	// radiator thermostats (RT-DN), each in configuration order.
	Thermostats []*Device
	// Contacts are the window/door contacts of the group.
	Contacts []*Device
	// Climate is the device providing the group's current temperature
	// (and humidity): the first wall thermostat if any, else the first
	// radiator thermostat.
	Climate *Device
	// CommandTargets are the thermostats that receive group commands:
	// all Thermostats, or only the wall thermostats if the group has
	// via_wall_thermostat set and contains one.
	CommandTargets []*Device
}

// Comment is ignored. JSON has no comments, so every object in the
// configuration accepts a "_comment" field holding a string or an array
// of strings (see example.json).
type Comment = any

// Duration is a time.Duration that unmarshals from a Go duration string.
type Duration struct {
	time.Duration
	set bool
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"1h\" or \"30m\"")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q (use e.g. \"30m\" or \"1h\")", s)
	}
	d.Duration = v
	d.set = true
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.Duration.String()) }

// Load reads and parses the configuration file at path. Insecure
// permissions of the file are reported by PermissionWarnings.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.permWarnings = append(permissionWarnings(path, fi), cfg.keyWarnings...)
	return cfg, nil
}

// PermissionWarnings returns the problems with the configuration file's
// permissions (readable by other users, owned by another user) found by
// Load, and those of the key files (security.key_file: readable by its
// group or other users). The files hold the AES system key: anyone who
// can read it can impersonate the central.
func (c *Config) PermissionWarnings() []string {
	if c.permWarnings == nil {
		return append([]string(nil), c.keyWarnings...) // Parse without Load
	}
	return append([]string(nil), c.permWarnings...)
}

// Parse parses and validates a JSON configuration.
func Parse(b []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, describeJSONError(b, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("invalid JSON: unexpected data after the top-level object")
	}
	if err := cfg.resolve(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func describeJSONError(b []byte, err error) error {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		line, col := lineCol(b, se.Offset)
		return fmt.Errorf("invalid JSON at line %d, column %d: %v", line, col, se)
	}
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		line, col := lineCol(b, te.Offset)
		return fmt.Errorf("line %d, column %d: field %q: cannot use JSON %s as %s", line, col, te.Field, te.Value, te.Type)
	}
	if msg := err.Error(); strings.HasPrefix(msg, "json: unknown field ") {
		return fmt.Errorf("unknown field %s (check spelling; see example.json)", strings.TrimPrefix(msg, "json: unknown field "))
	}
	return fmt.Errorf("invalid config: %v", err)
}

func lineCol(b []byte, off int64) (int, int) {
	if off > int64(len(b)) {
		off = int64(len(b))
	}
	line, col := 1, 1
	for _, c := range b[:off] {
		if c == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}

func parseAddr(s string) ([3]byte, error) {
	var a [3]byte
	t := strings.TrimSpace(s)
	t = strings.TrimPrefix(strings.TrimPrefix(t, "0x"), "0X")
	if len(t) != 6 {
		return a, fmt.Errorf("address %q: want 6 hex digits", s)
	}
	b, err := hex.DecodeString(t)
	if err != nil {
		return a, fmt.Errorf("address %q: not hexadecimal", s)
	}
	copy(a[:], b)
	return a, nil
}

// Placeholder is the prefix of the placeholder values hmgenconfig writes
// for settings it cannot know (passphrase, MQTT credentials).
const Placeholder = "CHANGE-ME"

func isPlaceholder(v string) bool { return strings.HasPrefix(strings.TrimSpace(v), Placeholder) }

func placeholderError(what string) error {
	return fmt.Errorf("%s: still the %s… placeholder from hmgenconfig; fill in the real value", what, Placeholder)
}

func (c *Config) resolveKey(what, key string, passphrase *string, keyFile string, index *int, defIndex int) (hmkey.Key, error) {
	if isPlaceholder(key) {
		return hmkey.Key{}, placeholderError(what + ".key")
	}
	if passphrase != nil && isPlaceholder(*passphrase) {
		return hmkey.Key{}, placeholderError(what + ".passphrase")
	}
	n := 0
	for _, set := range []bool{key != "", passphrase != nil, keyFile != ""} {
		if set {
			n++
		}
	}
	if n > 1 {
		return hmkey.Key{}, fmt.Errorf("%s: set only one of key, passphrase and key_file", what)
	}
	if n == 0 {
		return hmkey.Key{}, fmt.Errorf("%s: key, passphrase or key_file is required", what)
	}
	if keyFile != "" {
		k, warnings, err := readKeyFile(keyFile)
		if err != nil {
			return hmkey.Key{}, fmt.Errorf("%s.key_file: %w", what, err)
		}
		c.keyWarnings = append(c.keyWarnings, warnings...)
		key = k
	}
	idx := defIndex
	if index != nil {
		idx = *index
	}
	if idx < 0 || idx > 255 {
		return hmkey.Key{}, fmt.Errorf("%s: index %d out of range 0..255", what, idx)
	}
	if passphrase != nil {
		if *passphrase == "" {
			return hmkey.Key{}, fmt.Errorf("%s: passphrase must not be empty", what)
		}
		return hmkey.FromPassphrase(*passphrase, byte(idx)), nil
	}
	k, err := hmkey.FromHex(key, byte(idx))
	if err != nil {
		// hmkey's errors do not include the key itself, but be
		// defensive and only report the shape of the problem.
		return hmkey.Key{}, fmt.Errorf("%s: key must be 32 hex characters (16 bytes)", what)
	}
	return k, nil
}

func (c *Config) resolve() error {
	if c.SerialPort == "" {
		c.SerialPort = DefaultSerialPort
	}
	switch c.Radio {
	case "":
		c.Radio = RadioAuto
	case RadioAuto, RadioHMModRPIPCB, RadioRPIRFMod:
	default:
		return fmt.Errorf("radio: %q is not one of %q, %q, %q", c.Radio, RadioAuto, RadioHMModRPIPCB, RadioRPIRFMod)
	}
	switch c.Reset {
	case "", "auto":
		switch {
		case strings.HasPrefix(c.SerialPort, "/dev/raw-uart"):
			c.Reset = "raw-uart"
		case c.Radio == RadioRPIRFMod && !OnboardUART(c.SerialPort):
			// Not on the Pi's header (e.g. a USB adapter): its reset
			// line is not on a GPIO. The module is put into its
			// application via IDENTIFY/CHANGE_APP without a reset.
			c.Reset = "none"
		default:
			c.Reset = "gpio"
		}
	case "gpio", "raw-uart", "none":
	default:
		return fmt.Errorf("reset: %q is not one of \"auto\", \"gpio\", \"raw-uart\", \"none\"", c.Reset)
	}
	c.resetGPIOAuto = false
	if c.ResetGPIO == 0 {
		switch c.Radio {
		case RadioRPIRFMod:
			c.ResetGPIO = ResetGPIORPIRFMod
		case RadioAuto:
			c.resetGPIOAuto = true
			c.ResetGPIO = DefaultResetGPIO
		default:
			c.ResetGPIO = DefaultResetGPIO
		}
	}
	if c.ResetGPIO < 0 || c.ResetGPIO > 63 {
		return fmt.Errorf("reset_gpio: %d out of range 0..63", c.ResetGPIO)
	}

	if isPlaceholder(c.CentralAddress) {
		return placeholderError("central_address")
	}
	if c.CentralAddress == "" {
		return fmt.Errorf("central_address is required (the BidCoS address of your CCU, e.g. \"1A2B3C\")")
	}
	a, err := parseAddr(c.CentralAddress)
	if err != nil {
		return fmt.Errorf("central_address: %v", err)
	}
	if a == [3]byte{} {
		return fmt.Errorf("central_address must not be 000000")
	}
	c.centralAddr = a

	c.current = hmkey.EQ3Default
	c.previous = nil
	c.keyWarnings = nil
	if s := c.Security; s != nil {
		k, err := c.resolveKey("security", s.Key, s.Passphrase, s.KeyFile, s.Index, 1)
		if err != nil {
			return err
		}
		c.current = k
		if p := s.Previous; p != nil {
			// The CCU increments the key index on every key change, so
			// the previous key defaults to current index - 1.
			if p.Index == nil && k.Index == 0 {
				return fmt.Errorf("security.previous: index is required when the current index is 0")
			}
			pk, err := c.resolveKey("security.previous", p.Key, p.Passphrase, p.KeyFile, p.Index, int(k.Index)-1)
			if err != nil {
				return err
			}
			if pk.Index == k.Index {
				return fmt.Errorf("security.previous: index %d equals the current key's index", pk.Index)
			}
			c.previous = &pk
		}
	}

	if c.MQTT.Broker != nil && isPlaceholder(*c.MQTT.Broker) {
		return placeholderError("mqtt.broker")
	}
	if isPlaceholder(c.MQTT.Username) {
		return placeholderError("mqtt.username")
	}
	if isPlaceholder(c.MQTT.Password) {
		return placeholderError("mqtt.password")
	}
	if c.MQTT.ClientID == "" {
		c.MQTT.ClientID = DefaultClientID
	}
	if c.MQTT.BaseTopic == "" {
		c.MQTT.BaseTopic = DefaultBaseTopic
	}
	if strings.ContainsAny(c.MQTT.BaseTopic, "+#") {
		return fmt.Errorf("mqtt.base_topic: must not contain MQTT wildcards")
	}
	if c.MQTT.DiscoveryPrefix == "" {
		c.MQTT.DiscoveryPrefix = DefaultDiscoveryPrefix
	}
	if c.HTTPListen == nil {
		s := DefaultHTTPListen
		c.HTTPListen = &s
	}
	for i, h := range c.HTTPAllowedHosts {
		h = strings.TrimSpace(h)
		if h == "" || strings.ContainsAny(h, "/ \t@") {
			return fmt.Errorf("http_allowed_hosts: %q is not a host name", c.HTTPAllowedHosts[i])
		}
		c.HTTPAllowedHosts[i] = h
	}
	if !c.TimeSyncInterval.set {
		c.TimeSyncInterval.Duration = DefaultTimeSyncInterval
	}
	if c.TimeSyncInterval.Duration < time.Minute {
		return fmt.Errorf("time_sync_interval: %v is too short (minimum 1m)", c.TimeSyncInterval.Duration)
	}

	if c.StateSaveIntervalSetting.set && c.StateSaveIntervalSetting.Duration < time.Minute {
		return fmt.Errorf("state_save_interval: %v is too short (minimum 1m)", c.StateSaveIntervalSetting.Duration)
	}
	if !c.StatusPollIntervalSetting.set {
		c.StatusPollIntervalSetting.Duration = DefaultStatusPollInterval
	}
	if p := c.StatusPollIntervalSetting.Duration; p < 0 || p > 0 && p < 5*time.Minute {
		return fmt.Errorf("status_poll_interval: %v is too short (minimum 5m, or \"0s\" to disable)", p)
	}
	if c.StateDir != nil && *c.StateDir != "" {
		if isPlaceholder(*c.StateDir) {
			return placeholderError("state_dir")
		}
		if !filepath.IsAbs(*c.StateDir) {
			return fmt.Errorf("state_dir: %q is not an absolute path", *c.StateDir)
		}
	}
	if c.StateFile != nil && *c.StateFile != "" && isPlaceholder(*c.StateFile) {
		return placeholderError("state_file")
	}
	if c.DevicesFile != nil && *c.DevicesFile != "" && isPlaceholder(*c.DevicesFile) {
		return placeholderError("devices_file")
	}
	if len(c.Devices) == 0 && c.DevicesFilePath() == "" {
		return fmt.Errorf("devices: at least one device is required (or set state_dir and pair devices with hmbridge)")
	}
	if p := c.Pairing; p != nil {
		switch p.AES {
		case "", PairingAESOff:
		case PairingAESSystemKey:
			if c.Security == nil {
				return fmt.Errorf("pairing.aes: %q needs a security block (the key to give the devices)", PairingAESSystemKey)
			}
		default:
			return fmt.Errorf("pairing.aes: %q is not one of %q, %q", p.AES, PairingAESSystemKey, PairingAESOff)
		}
		if d := p.Duration; d.set && (d.Duration < 10*time.Second || d.Duration > 10*time.Minute) {
			return fmt.Errorf("pairing.duration: %v out of range 10s … 10m", d.Duration)
		}
	}
	byName := make(map[string]int)
	byAddr := make(map[[3]byte]int)
	for i := range c.Devices {
		d := &c.Devices[i]
		where := fmt.Sprintf("devices[%d]", i)
		if d.Name != "" {
			where = fmt.Sprintf("devices[%d] (%q)", i, d.Name)
		}
		if err := c.resolveDevice(d, where, false); err != nil {
			return err
		}
		if j, ok := byName[strings.ToLower(d.Name)]; ok {
			return fmt.Errorf("%s: duplicate name (also used by devices[%d])", where, j)
		}
		byName[strings.ToLower(d.Name)] = i
		if j, ok := byAddr[d.addr]; ok {
			return fmt.Errorf("%s: duplicate address %s (also used by devices[%d])", where, d.AddrHex(), j)
		}
		byAddr[d.addr] = i
	}

	inGroup := make(map[int]string)
	groupNames := make(map[string]bool)
	for gi := range c.Groups {
		g := &c.Groups[gi]
		where := fmt.Sprintf("groups[%d]", gi)
		if strings.TrimSpace(g.Name) == "" {
			return fmt.Errorf("%s: name is required", where)
		}
		where = fmt.Sprintf("groups[%d] (%q)", gi, g.Name)
		if groupNames[strings.ToLower(g.Name)] {
			return fmt.Errorf("%s: duplicate group name", where)
		}
		groupNames[strings.ToLower(g.Name)] = true
		g.thermostats, g.contacts = nil, nil
		var tcs, rts []int
		for _, m := range g.Members {
			idx, ok := byName[strings.ToLower(m)]
			if !ok {
				if a, err := parseAddr(m); err == nil {
					idx, ok = byAddr[a]
				}
			}
			if !ok {
				return fmt.Errorf("%s: member %q is not a configured device name or address", where, m)
			}
			if other, dup := inGroup[idx]; dup {
				return fmt.Errorf("%s: device %q is already a member of group %q", where, c.Devices[idx].Name, other)
			}
			inGroup[idx] = g.Name
			switch md := c.Devices[idx].Model; {
			case md == ModelTC:
				tcs = append(tcs, idx)
			case md == ModelRTDN:
				rts = append(rts, idx)
			case IsContact(md):
				g.contacts = append(g.contacts, idx)
			default:
				return fmt.Errorf("%s: member %q is a %s; a heating group takes only thermostats and window contacts", where, c.Devices[idx].Name, md)
			}
		}
		if len(tcs)+len(rts) == 0 {
			return fmt.Errorf("%s: a group needs at least one thermostat (%s or %s)", where, ModelRTDN, ModelTC)
		}
		g.thermostats = append(tcs, rts...)
	}
	return nil
}

// resolveDevice validates d and fills in its parsed fields. A device
// from the devices file may also use key index 0 (the eQ-3 default
// key, which every device has after a factory reset).
func (c *Config) resolveDevice(d *Device, where string, fromStore bool) error {
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("%s: name is required", where)
	}
	if _, ok := modelChannels[d.Model]; !ok {
		return fmt.Errorf("%s: unsupported model %q (supported: %s)", where, d.Model, strings.Join(Models(), ", "))
	}
	if d.Address == "" {
		return fmt.Errorf("%s: address is required", where)
	}
	a, err := parseAddr(d.Address)
	if err != nil {
		return fmt.Errorf("%s: %v", where, err)
	}
	if a == [3]byte{} {
		return fmt.Errorf("%s: address must not be 000000", where)
	}
	if a == c.centralAddr {
		return fmt.Errorf("%s: address equals central_address", where)
	}
	d.addr = a
	if d.Serial != "" && len(d.Serial) != 10 {
		return fmt.Errorf("%s: serial %q: want 10 characters", where, d.Serial)
	}
	if d.AESEnabled() && !SupportsAES(d.Model) {
		return fmt.Errorf("%s: %s does not support AES; remove aes/aes_channels", where, d.Model)
	}
	for _, ch := range d.AESChannels {
		if ch < 1 || ch > d.Channels() {
			return fmt.Errorf("%s: aes_channels: channel %d out of range 1..%d for %s (channel 0 is the maintenance channel)", where, ch, d.Channels(), d.Model)
		}
	}
	if d.StaleAfter.set && d.StaleAfter.Duration != 0 && d.StaleAfter.Duration < time.Minute {
		return fmt.Errorf("%s: stale_after %v is too short (minimum 1m, or \"0s\" for never)", where, d.StaleAfter.Duration)
	}
	if poll := c.StatusPollIntervalSetting.Duration; IsActuator(d.Model) && poll > 0 &&
		d.StaleAfter.set && d.StaleAfter.Duration != 0 && d.StaleAfter.Duration < poll+StalePollMargin {
		return fmt.Errorf("%s: stale_after %v must be at least status_poll_interval %v + %v (the actuator is only heard from when polled, and a poll round takes time), or \"0s\" for never",
			where, d.StaleAfter.Duration, poll, StalePollMargin)
	}
	d.staleDefault = defaultStale(d.Model, c.StatusPollIntervalSetting.Duration)
	d.keyIndex = c.current.Index
	if d.KeyIndexOverride != nil {
		ki := *d.KeyIndexOverride
		valid := ki == int(c.current.Index) || (c.previous != nil && ki == int(c.previous.Index)) || (fromStore && ki == 0)
		if !valid {
			known := fmt.Sprintf("%d (current)", c.current.Index)
			if c.previous != nil {
				known += fmt.Sprintf(" or %d (previous)", c.previous.Index)
			}
			return fmt.Errorf("%s: key_index %d is not the index of a configured key; must be %s", where, ki, known)
		}
		d.keyIndex = byte(ki)
	}
	d.fromStore = fromStore
	return nil
}

// ResolveStoreDevice validates a device from the devices file (paired
// with hmbridge) against the configuration and returns it ready for
// use. Unlike configured devices, it may use key index 0. It does not
// check for conflicts with other devices; see ConflictsWith.
func (c *Config) ResolveStoreDevice(d Device) (*Device, error) {
	where := fmt.Sprintf("device %s", d.Address)
	if d.Name != "" {
		where = fmt.Sprintf("device %s (%q)", d.Address, d.Name)
	}
	if err := c.resolveDevice(&d, where, true); err != nil {
		return nil, err
	}
	return &d, nil
}

// ConfiguredDevice returns the device of the configuration file (not
// the devices file) with the given address or, if addr matches none,
// with the given name (case-insensitive), or nil.
func (c *Config) ConfiguredDevice(addr [3]byte, name string) *Device {
	if d := c.Device(addr); d != nil {
		return d
	}
	for i := range c.Devices {
		if name != "" && strings.EqualFold(c.Devices[i].Name, name) {
			return &c.Devices[i]
		}
	}
	return nil
}

// StateDirPath returns the resolved state directory ("" = none).
func (c *Config) StateDirPath() string {
	switch {
	case c.StateDir == nil:
		return DefaultStateDir
	case *c.StateDir == "":
		return ""
	}
	return filepath.Clean(*c.StateDir)
}

// filePath resolves a file of the state directory: the deprecated
// explicit path if set, else <state_dir>/<name> ("" = disabled).
func (c *Config) filePath(explicit *string, name string) string {
	if explicit != nil {
		return *explicit
	}
	if dir := c.StateDirPath(); dir != "" {
		return filepath.Join(dir, name)
	}
	return ""
}

// DevicesFilePath returns the resolved devices file path ("" = pairing
// disabled).
func (c *Config) DevicesFilePath() string { return c.filePath(c.DevicesFile, DevicesFileName) }

// Deprecations returns a message for every deprecated option in use
// (hmbridge logs them once at startup).
func (c *Config) Deprecations() []string {
	var msgs []string
	for _, o := range []struct {
		key, name string
		val       *string
	}{{"state_file", StateFileName, c.StateFile}, {"devices_file", DevicesFileName, c.DevicesFile}} {
		if o.val == nil {
			continue
		}
		m := fmt.Sprintf("%s is deprecated: use state_dir (hmbridge keeps %s there)", o.key, o.name)
		if *o.val == "" {
			m += fmt.Sprintf("; %s \"\" still disables the file", o.key)
		} else {
			m += fmt.Sprintf("; %q is still used for now; to switch, move it to <state_dir>/%s and remove %s", *o.val, o.name, o.key)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

// PairingAES returns the resolved pairing AES mode: pairing.aes, or
// "system_key" when a security block is configured, else "off".
func (c *Config) PairingAES() string {
	if c.Pairing != nil && c.Pairing.AES != "" {
		return c.Pairing.AES
	}
	if c.Security != nil {
		return PairingAESSystemKey
	}
	return PairingAESOff
}

// PairingDuration returns how long pairing mode stays on.
func (c *Config) PairingDuration() time.Duration {
	if c.Pairing != nil && c.Pairing.Duration.set {
		return c.Pairing.Duration.Duration
	}
	return DefaultPairingDuration
}

// StateFilePath returns the resolved state file path ("" = disabled).
func (c *Config) StateFilePath() string { return c.filePath(c.StateFile, StateFileName) }

// DefaultStateSaveInterval is the default minimum time between writes.
const DefaultStateSaveInterval = 15 * time.Minute

// StateSaveInterval returns the resolved minimum time between writes of
// the state file.
func (c *Config) StateSaveInterval() time.Duration {
	if c.StateSaveIntervalSetting.Duration > 0 {
		return c.StateSaveIntervalSetting.Duration
	}
	return DefaultStateSaveInterval
}

// SuggestGroupAreas reports whether heating group names are sent as
// suggested areas (groups_suggest_area, default true).
func (c *Config) SuggestGroupAreas() bool {
	return c.GroupsSuggestArea == nil || *c.GroupsSuggestArea
}

// RequiresSyncedClock reports whether time is only handed out while the
// system clock is NTP-synchronised (default true).
func (c *Config) RequiresSyncedClock() bool {
	return c.RequireSyncedClock == nil || *c.RequireSyncedClock
}

// StalePollMargin is how much longer than status_poll_interval an
// actuator's stale_after must be at least: polls are spread out and
// retried, so an answer can arrive a while after the interval.
const StalePollMargin = 5 * time.Minute

// DefaultStatusPollInterval is the default status_poll_interval.
const DefaultStatusPollInterval = time.Hour

// StatusPollInterval returns how often actuators are polled (0 =
// never, except once at startup).
func (c *Config) StatusPollInterval() time.Duration { return c.StatusPollIntervalSetting.Duration }

func defaultStale(model string, poll time.Duration) time.Duration {
	switch {
	case model == ModelRTDN, model == ModelTC:
		return 30 * time.Minute
	case model == ModelSCo:
		return 6 * time.Hour
	case IsWeather(model):
		return time.Hour // they report every ~3 minutes
	case IsRemote(model):
		return 0 // silent until a button is pressed
	case IsActuator(model):
		if poll <= 0 {
			return 0
		}
		return 3*poll + 5*time.Minute
	}
	return 50 * time.Hour
}

// StaleTimeout returns how long the device may be silent before it is
// reported unavailable; 0 means never.
func (d Device) StaleTimeout() time.Duration {
	if d.StaleAfter.set {
		return d.StaleAfter.Duration
	}
	if d.staleDefault == 0 && d.addr == ([3]byte{}) {
		// Not resolved (a Device built outside Parse).
		return defaultStale(d.Model, DefaultStatusPollInterval)
	}
	return d.staleDefault
}

// AnswersTimeRequests reports whether hmbridge answers time requests
// (default true).
func (c *Config) AnswersTimeRequests() bool {
	return c.AnswerTimeRequests == nil || *c.AnswerTimeRequests
}

// CentralAddr returns the parsed central address.
func (c *Config) CentralAddr() [3]byte { return c.centralAddr }

// Keys returns the current AES key (hmkey.EQ3Default when no security
// block is configured) and the previous key (nil unless configured).
func (c *Config) Keys() (current, previous *hmkey.Key) {
	cur := c.current
	var prev *hmkey.Key
	if c.previous != nil {
		p := *c.previous
		prev = &p
	}
	return &cur, prev
}

// HTTPListenAddr returns the resolved HTTP listen address ("" =
// disabled).
func (c *Config) HTTPListenAddr() string {
	if c.HTTPListen == nil {
		return DefaultHTTPListen
	}
	return *c.HTTPListen
}

// Device returns the device with the given address, or nil.
func (c *Config) Device(addr [3]byte) *Device {
	for i := range c.Devices {
		if c.Devices[i].addr == addr {
			return &c.Devices[i]
		}
	}
	return nil
}

// ResolvedGroups returns the configured groups with members resolved.
func (c *Config) ResolvedGroups() []ResolvedGroup {
	res := make([]ResolvedGroup, 0, len(c.Groups))
	for _, g := range c.Groups {
		rg := ResolvedGroup{Name: g.Name}
		for _, i := range g.thermostats {
			rg.Thermostats = append(rg.Thermostats, &c.Devices[i])
		}
		for _, i := range g.contacts {
			rg.Contacts = append(rg.Contacts, &c.Devices[i])
		}
		if len(rg.Thermostats) > 0 {
			rg.Climate = rg.Thermostats[0]
		}
		for _, d := range rg.Thermostats {
			if !g.ViaWallThermostat || rg.Climate.Model != ModelTC || d.Model == ModelTC {
				rg.CommandTargets = append(rg.CommandTargets, d)
			}
		}
		res = append(res, rg)
	}
	return res
}

// GroupOf returns the name of the group addr belongs to, or "".
func (c *Config) GroupOf(addr [3]byte) string {
	for _, g := range c.ResolvedGroups() {
		for _, d := range append(append([]*Device{}, g.Thermostats...), g.Contacts...) {
			if d.addr == addr {
				return g.Name
			}
		}
	}
	return ""
}

// ResetGPIOAuto reports whether the reset line is chosen at startup:
// reset_gpio is not set and the radio module is detected ("radio":
// "auto"). ResetGPIO then holds the HM-MOD-RPI-PCB's line, the
// fallback.
func (c *Config) ResetGPIOAuto() bool { return c.resetGPIOAuto }

// OnboardUART reports whether port is the Raspberry Pi's own UART on
// the GPIO header (where the radio modules' reset lines are GPIOs).
func OnboardUART(port string) bool {
	for _, p := range []string{"/dev/serial0", "/dev/serial1", "/dev/ttyAMA", "/dev/ttyS"} {
		if strings.HasPrefix(port, p) {
			return true
		}
	}
	return false
}
