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
)

// modelChannels maps each supported model to its highest channel
// number (channel 0 is the maintenance channel and always exists).
// Taken from the channel lists in eQ-3's rftypes XML
// (rf_cc_rt_dn.xml, rf_tc_it_wm-w-eu.xml, rf_sc.xml, rf_sec_sco.xml,
// rf_rhs.xml).
var modelChannels = map[string]int{
	ModelRTDN: 6, // 1 weather rx, 2 climate rx, 3 window rx, 4 climate (control), 5 RT team, 6 remote
	ModelTC:   7, // 1 weather tx, 2 thermal control (control), 3 window rx, 6 remote, 7 switch tx
	ModelSC2:  1,
	ModelSC:   1,
	ModelSCo:  1,
	ModelRHS:  1,
}

// Models returns the supported model names.
func Models() []string {
	return []string{ModelRTDN, ModelTC, ModelSC2, ModelSC, ModelSCo, ModelRHS}
}

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
	DefaultResetGPIO        = 18
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
	// (HM-MOD-RPI-PCB reset line on reset_gpio), "raw-uart" (ask
	// OpenCCU's raw-uart kernel driver, which knows the line of either
	// module), "none", or "auto" (default: "raw-uart" for /dev/raw-uart*,
	// else "gpio" for an HM-MOD-RPI-PCB and "none" otherwise).
	Reset string `json:"reset"`
	// Radio selects the radio module: "auto" (default, detected at
	// startup), "hm-mod-rpi-pcb" or "rpi-rf-mod".
	Radio     string `json:"radio,omitempty"`
	ResetGPIO int    `json:"reset_gpio"` // BCM GPIO number, default 18 (0 = default)

	// CentralAddress is the BidCoS address of the CCU to impersonate
	// (6 hex digits, optional 0x prefix). Required.
	CentralAddress string `json:"central_address"`

	// Security is optional; absent means the eQ-3 default key (index 0).
	Security *Security `json:"security,omitempty"`

	MQTT MQTT `json:"mqtt"`

	// HTTPListen is the listen address of the status page and
	// /metrics. Default ":8013"; "" disables the HTTP server.
	HTTPListen *string `json:"http_listen,omitempty"`

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

	// StateFile stores the last known device state, so that a restart
	// does not blank Home Assistant until every device has reported
	// again (window contacts report only every few hours). Default
	// "/var/lib/hmbridge/state.json"; "" disables it.
	StateFile *string `json:"state_file,omitempty"`

	// StateSaveInterval is the minimum time between two writes of the
	// state file (Go duration, default "15m"). The file is only written
	// when something significant changed (mode, target, window, battery,
	// fault), and once on shutdown; normal temperature reports never
	// cause a write. The broker's retained messages are the primary
	// source after a restart, so this only matters if the broker is
	// unreachable at startup.
	StateSaveIntervalSetting Duration `json:"state_save_interval,omitempty"`

	Devices []Device `json:"devices"`
	Groups  []Group  `json:"groups,omitempty"`

	centralAddr [3]byte
	current     hmkey.Key
	previous    *hmkey.Key
}

// Security configures the AES key used by the devices (the CCU's
// “system security key”). Exactly one of Key and Passphrase may be set.
type Security struct {
	Comment    Comment  `json:"_comment,omitempty"`
	Key        string   `json:"key,omitempty"`        // 32 hex chars
	Passphrase *string  `json:"passphrase,omitempty"` // CCU passphrase; key = MD5(passphrase)
	Index      *int     `json:"index,omitempty"`      // key index, default 1 when key/passphrase is set
	Previous   *KeySpec `json:"previous,omitempty"`   // optional older key
}

// KeySpec is a key given either as hex or as passphrase.
type KeySpec struct {
	Comment    Comment `json:"_comment,omitempty"`
	Key        string  `json:"key,omitempty"`
	Passphrase *string `json:"passphrase,omitempty"`
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
	// Assistant shows it as unavailable (Go duration). Default: 30m for
	// thermostats (they report every few minutes), 6h for the
	// HM-Sec-SCo, 50h for the other contacts (they report about daily).
	StaleAfter Duration `json:"stale_after,omitempty"`

	addr     [3]byte
	keyIndex byte
}

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

// Load reads and parses the configuration file at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
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

func resolveKey(what, key string, passphrase *string, index *int, defIndex int) (hmkey.Key, error) {
	if key != "" && passphrase != nil {
		return hmkey.Key{}, fmt.Errorf("%s: set either key or passphrase, not both", what)
	}
	if key == "" && passphrase == nil {
		return hmkey.Key{}, fmt.Errorf("%s: key or passphrase is required", what)
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
		case c.Radio == RadioRPIRFMod:
			// The RPI-RF-MOD's reset line differs from the
			// HM-MOD-RPI-PCB's; without the raw-uart driver, start
			// without a reset (the module is put into its app via
			// IDENTIFY/CHANGE_APP).
			c.Reset = "none"
		default:
			c.Reset = "gpio"
		}
	case "gpio", "raw-uart", "none":
	default:
		return fmt.Errorf("reset: %q is not one of \"auto\", \"gpio\", \"raw-uart\", \"none\"", c.Reset)
	}
	if c.ResetGPIO == 0 {
		c.ResetGPIO = DefaultResetGPIO
	}
	if c.ResetGPIO < 0 || c.ResetGPIO > 63 {
		return fmt.Errorf("reset_gpio: %d out of range 0..63", c.ResetGPIO)
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
	if s := c.Security; s != nil {
		k, err := resolveKey("security", s.Key, s.Passphrase, s.Index, 1)
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
			pk, err := resolveKey("security.previous", p.Key, p.Passphrase, p.Index, int(k.Index)-1)
			if err != nil {
				return err
			}
			if pk.Index == k.Index {
				return fmt.Errorf("security.previous: index %d equals the current key's index", pk.Index)
			}
			c.previous = &pk
		}
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
	if !c.TimeSyncInterval.set {
		c.TimeSyncInterval.Duration = DefaultTimeSyncInterval
	}
	if c.TimeSyncInterval.Duration < time.Minute {
		return fmt.Errorf("time_sync_interval: %v is too short (minimum 1m)", c.TimeSyncInterval.Duration)
	}

	if c.StateSaveIntervalSetting.set && c.StateSaveIntervalSetting.Duration < time.Minute {
		return fmt.Errorf("state_save_interval: %v is too short (minimum 1m)", c.StateSaveIntervalSetting.Duration)
	}
	if len(c.Devices) == 0 {
		return fmt.Errorf("devices: at least one device is required")
	}
	byName := make(map[string]int)
	byAddr := make(map[[3]byte]int)
	for i := range c.Devices {
		d := &c.Devices[i]
		where := fmt.Sprintf("devices[%d]", i)
		if d.Name != "" {
			where = fmt.Sprintf("devices[%d] (%q)", i, d.Name)
		}
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
		for _, ch := range d.AESChannels {
			if ch < 1 || ch > d.Channels() {
				return fmt.Errorf("%s: aes_channels: channel %d out of range 1..%d for %s (channel 0 is the maintenance channel)", where, ch, d.Channels(), d.Model)
			}
		}
		if d.StaleAfter.set && d.StaleAfter.Duration < time.Minute {
			return fmt.Errorf("%s: stale_after %v is too short (minimum 1m)", where, d.StaleAfter.Duration)
		}
		d.keyIndex = c.current.Index
		if d.KeyIndexOverride != nil {
			ki := *d.KeyIndexOverride
			valid := ki == int(c.current.Index) || (c.previous != nil && ki == int(c.previous.Index))
			if !valid {
				known := fmt.Sprintf("%d (current)", c.current.Index)
				if c.previous != nil {
					known += fmt.Sprintf(" or %d (previous)", c.previous.Index)
				}
				return fmt.Errorf("%s: key_index %d is not the index of a configured key; must be %s", where, ki, known)
			}
			d.keyIndex = byte(ki)
		}
		if j, ok := byName[strings.ToLower(d.Name)]; ok {
			return fmt.Errorf("%s: duplicate name (also used by devices[%d])", where, j)
		}
		byName[strings.ToLower(d.Name)] = i
		if j, ok := byAddr[a]; ok {
			return fmt.Errorf("%s: duplicate address %s (also used by devices[%d])", where, d.AddrHex(), j)
		}
		byAddr[a] = i
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
			default:
				g.contacts = append(g.contacts, idx)
			}
		}
		if len(tcs)+len(rts) == 0 {
			return fmt.Errorf("%s: a group needs at least one thermostat (%s or %s)", where, ModelRTDN, ModelTC)
		}
		g.thermostats = append(tcs, rts...)
	}
	return nil
}

// DefaultStateFile is where the last known device state is kept.
const DefaultStateFile = "/var/lib/hmbridge/state.json"

// StateFilePath returns the resolved state file path ("" = disabled).
func (c *Config) StateFilePath() string {
	if c.StateFile == nil {
		return DefaultStateFile
	}
	return *c.StateFile
}

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

// RequiresSyncedClock reports whether time is only handed out while the
// system clock is NTP-synchronised (default true).
func (c *Config) RequiresSyncedClock() bool {
	return c.RequireSyncedClock == nil || *c.RequireSyncedClock
}

// StaleTimeout returns how long the device may be silent before it is
// reported unavailable.
func (d Device) StaleTimeout() time.Duration {
	if d.StaleAfter.Duration > 0 {
		return d.StaleAfter.Duration
	}
	switch d.Model {
	case ModelRTDN, ModelTC:
		return 30 * time.Minute
	case ModelSCo:
		return 6 * time.Hour
	}
	return 50 * time.Hour
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
