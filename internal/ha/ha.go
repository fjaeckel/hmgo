// Package ha exposes HomeMatic thermostats, window contacts and heating
// groups to Home Assistant via MQTT.
//
// The package is decoupled from the BidCoS device packages: the bridge
// converts device events into State values and Command values back into
// device commands.
//
// # Topics
//
// With the default TopicConfig (base "hmbridge", discovery prefix
// "homeassistant"):
//
//	hmbridge/bridge/availability        "online" / "offline" (retained, LWT)
//	hmbridge/<addr>/state               retained JSON State
//	hmbridge/<addr>/set/temperature     "21.5"  (4.5 … 30.5, rounded to 0.5)
//	hmbridge/<addr>/set/mode            "auto" | "heat" | "off"
//	hmbridge/<addr>/set/preset          "boost" | "none"
//	hmbridge/group/<slug>/state         retained JSON State of a heating group
//	hmbridge/group/<slug>/set/...       as for devices
//	homeassistant/status                HA birth/will message ("online")
//
// <addr> is the BidCoS address as 6 lowercase hex digits, e.g. "38f59c".
//
// # Discovery
//
// DiscoveryMessages returns one retained config message per entity:
//
//	homeassistant/climate/hmbridge_<addr>/config         (thermostats only)
//	homeassistant/sensor/hmbridge_<addr>/<key>/config
//	homeassistant/binary_sensor/hmbridge_<addr>/<key>/config
//
// GroupDiscoveryMessages does the same for a heating group, with node id
// "hmbridge_group_<slug>": a climate entity and a window binary sensor on
// an HA device "<name> (group)", model "Heating group". Member devices
// stay independent HA devices (no via_device: a group is not a hub).
//
// All entities belong to one HA device (identifier "hmbridge_<addr>") and
// read the same state topic through value templates. Templates tolerate
// missing JSON fields: a missing number renders as "" (HA ignores the
// update), a missing mode/fault renders as "None" (HA shows unknown).
// unique_id is "hmbridge_<addr>[_<key>]" and default_entity_id (HA
// ≥ 2025.10) is "<domain>.hmbridge_<addr>[_<key>]", so entity IDs are
// stable and address-based. The deprecated object_id is not sent.
//
// # Modes
//
// HM auto → HA "auto"; HM manual → "heat", or "off" when the target is
// 4.5 °C; boost is HA preset "boost" (HA mode "heat"); party is shown as
// "auto" (no preset). See HAMode. Commands: mode "off" means manual 4.5 °C,
// mode "heat" means manual mode at the current target temperature, preset
// "none" means leaving boost (the bridge decides how, e.g. back to auto).
//
// # Client
//
// Connect returns a Client that reconnects automatically. On every
// (re)connect, and whenever HA publishes "online" on homeassistant/status,
// the Client publishes availability "online", (re)subscribes to command
// topics and calls Options.OnConnect, in which the bridge should publish
// DiscoveryMessages and the last StateMessage for every device. OnConnect
// calls are serialized and coalesced. Commands are delivered on
// Client.Commands; invalid ones are logged and dropped.
package ha

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Kind is the device type.
type Kind int

const (
	// RadiatorThermostat is an HM-CC-RT-DN radiator valve drive.
	RadiatorThermostat Kind = iota
	// WallThermostat is an HM-TC-IT-WM-W-EU wall thermostat.
	WallThermostat
	// WindowContact is an HM-Sec-SC-2 (two-state, with sabotage contact),
	// an HM-Sec-SCo (optical, two-state)
	// or HM-Sec-RHS (three-state: closed/tilted/open) window contact.
	// Contacts report low battery only (no voltage) and have no climate
	// entity.
	WindowContact
	// HeatingGroup is a CCU-style heating group (see GroupDesc). It is
	// not used in DeviceDesc; use the Group* functions instead.
	HeatingGroup
)

func (k Kind) String() string {
	switch k {
	case RadiatorThermostat:
		return "RadiatorThermostat"
	case WallThermostat:
		return "WallThermostat"
	case WindowContact:
		return "WindowContact"
	case HeatingGroup:
		return "HeatingGroup"
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// KindForModel maps a HomeMatic model name to a Kind.
func KindForModel(model string) (Kind, bool) {
	switch strings.ToUpper(model) {
	case "HM-CC-RT-DN":
		return RadiatorThermostat, true
	case "HM-TC-IT-WM-W-EU":
		return WallThermostat, true
	case "HM-SEC-SC-2", "HM-SEC-SC", "HM-SEC-SCO", "HM-SEC-RHS":
		return WindowContact, true
	}
	return 0, false
}

// IsThreeState reports whether a window contact model reports
// closed/tilted/open (HM-Sec-RHS) rather than closed/open.
func IsThreeState(model string) bool {
	return strings.HasPrefix(strings.ToUpper(model), "HM-SEC-RHS")
}

// DeviceDesc describes one device for discovery.
type DeviceDesc struct {
	Addr   string // lowercase hex, e.g. "38f59c"
	Name   string // HA device name, e.g. "Wohnzimmer"
	Model  string // e.g. "HM-CC-RT-DN"
	Serial string // optional
	Kind   Kind   // RadiatorThermostat, WallThermostat or WindowContact
}

// GroupDesc describes a heating group for discovery. Group topics live
// under <base>/group/<slug>/.
type GroupDesc struct {
	Slug string // [a-z0-9_-]{1,64}, e.g. "wohnzimmer"
	Name string // e.g. "Wohnzimmer"; the HA device is named "<Name> (group)"
}

// ValidSlug reports whether s can be used as a GroupDesc.Slug.
func ValidSlug(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// Slugify turns a group name into a valid slug ("Wohnzimmer Süd" →
// "wohnzimmer_s_d"). Names without usable characters yield "group".
func Slugify(name string) string {
	var b strings.Builder
	lastUnderscore := true
	for _, c := range strings.ToLower(name) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' {
			b.WriteRune(c)
			lastUnderscore = false
		} else if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
		if b.Len() >= 64 {
			break
		}
	}
	s := strings.Trim(b.String(), "_")
	if s == "" {
		return "group"
	}
	return s
}

// Temperature limits of the devices. 4.5 means off, 30.5 means on (valve
// fully open).
const (
	MinTemp  = 4.5
	MaxTemp  = 30.5
	TempStep = 0.5
)

// HA climate modes, presets and actions.
const (
	ModeAuto = "auto"
	ModeHeat = "heat"
	ModeOff  = "off"

	PresetBoost = "boost"
	PresetNone  = "none"

	ActionHeating = "heating"
	ActionIdle    = "idle"
	ActionOff     = "off"

	FaultNone = "none"
)

// State is the (possibly partial) state of a device. Nil pointers, empty
// strings and a zero LastSeen mean “unknown / not part of this update”.
type State struct {
	CurrentTemperature *float64
	TargetTemperature  *float64
	Mode               string // "auto" | "heat" | "off"
	Preset             string // "boost" | "none"
	Action             string // "heating" | "idle" | "off"
	ValvePosition      *int   // percent
	BatteryVoltage     *float64
	LowBattery         *bool
	Humidity           *int // percent
	WindowOpen         *bool
	Fault              string // "none" when OK, otherwise the device's fault text
	RSSI               *int   // dBm
	LastSeen           time.Time

	// ControlMode is the raw HomeMatic control mode: "auto", "manual",
	// "party" or "boost".
	ControlMode string
	// BoostMinutes is the remaining boost time in minutes (RT-DN).
	BoostMinutes *int
	// BoostState is the raw boost state value reported by the TC.
	BoostState *int
	// CommReporting is the TC's “communication reporting” flag.
	CommReporting *bool
	// AuthStatus describes the AES authentication of the last message,
	// e.g. "ok", "failed" or "none" (not authenticated).
	AuthStatus string
	// MessagesReceived counts messages received from the device since
	// the bridge started.
	MessagesReceived *uint64
	// WindowState is the raw contact state: "closed", "tilted" (RHS
	// only) or "open". Set WindowOpen too (open = tilted or open).
	WindowState string
	// LastCommand describes the outcome of the last command sent to the
	// device, e.g. "ok: set_temperature 21.5" or "failed: boost (no
	// response from device)", so failures are visible in Home Assistant.
	LastCommand string
	// Sabotage is the tamper contact of a window contact (all supported
	// models report it, per eQ-3's rftypes XML).
	Sabotage *bool
}

// Window contact states.
const (
	WindowClosed = "closed"
	WindowTilted = "tilted"
	WindowOpened = "open"
)

// Battery percentage estimate: both models run on 2×1.5 V cells; the
// estimate is linear between BatteryEmptyVoltage (0 %) and
// BatteryFullVoltage (100 %), clamped. It is only a rough estimate
// (alkaline discharge curves are not linear).
const (
	BatteryEmptyVoltage = 2.2
	BatteryFullVoltage  = 3.0
)

// BatteryPercent estimates the battery level from its voltage, see
// BatteryEmptyVoltage. The result is rounded to an integer percentage.
func BatteryPercent(voltage float64) int {
	p := (voltage - BatteryEmptyVoltage) / (BatteryFullVoltage - BatteryEmptyVoltage) * 100
	p = math.Round(p)
	if p < 0 || math.IsNaN(p) {
		return 0
	}
	if p > 100 {
		return 100
	}
	return int(p)
}

// Merge overwrites the fields of s with all known fields of update.
func (s *State) Merge(u State) {
	if u.CurrentTemperature != nil {
		s.CurrentTemperature = ptr(*u.CurrentTemperature)
	}
	if u.TargetTemperature != nil {
		s.TargetTemperature = ptr(*u.TargetTemperature)
	}
	if u.Mode != "" {
		s.Mode = u.Mode
	}
	if u.Preset != "" {
		s.Preset = u.Preset
	}
	if u.Action != "" {
		s.Action = u.Action
	}
	if u.ValvePosition != nil {
		s.ValvePosition = ptr(*u.ValvePosition)
	}
	if u.BatteryVoltage != nil {
		s.BatteryVoltage = ptr(*u.BatteryVoltage)
	}
	if u.LowBattery != nil {
		s.LowBattery = ptr(*u.LowBattery)
	}
	if u.Humidity != nil {
		s.Humidity = ptr(*u.Humidity)
	}
	if u.WindowOpen != nil {
		s.WindowOpen = ptr(*u.WindowOpen)
	}
	if u.Fault != "" {
		s.Fault = u.Fault
	}
	if u.RSSI != nil {
		s.RSSI = ptr(*u.RSSI)
	}
	if !u.LastSeen.IsZero() {
		s.LastSeen = u.LastSeen
	}
	if u.ControlMode != "" {
		s.ControlMode = u.ControlMode
	}
	if u.BoostMinutes != nil {
		s.BoostMinutes = ptr(*u.BoostMinutes)
	}
	if u.BoostState != nil {
		s.BoostState = ptr(*u.BoostState)
	}
	if u.CommReporting != nil {
		s.CommReporting = ptr(*u.CommReporting)
	}
	if u.AuthStatus != "" {
		s.AuthStatus = u.AuthStatus
	}
	if u.LastCommand != "" {
		s.LastCommand = u.LastCommand
	}
	if u.MessagesReceived != nil {
		s.MessagesReceived = ptr(*u.MessagesReceived)
	}
	if u.WindowState != "" {
		s.WindowState = u.WindowState
	}
	if u.Sabotage != nil {
		s.Sabotage = ptr(*u.Sabotage)
	}
}

func ptr[T any](v T) *T { return &v }

// stateJSON is the wire format of State; unknown fields are omitted.
type stateJSON struct {
	CurrentTemperature *float64 `json:"current_temperature,omitempty"`
	TargetTemperature  *float64 `json:"target_temperature,omitempty"`
	Mode               string   `json:"mode,omitempty"`
	Preset             string   `json:"preset,omitempty"`
	Action             string   `json:"action,omitempty"`
	ValvePosition      *int     `json:"valve_position,omitempty"`
	BatteryVoltage     *float64 `json:"battery_voltage,omitempty"`
	BatteryPercent     *int     `json:"battery_percent,omitempty"` // derived, estimate
	LowBattery         *bool    `json:"low_battery,omitempty"`
	Humidity           *int     `json:"humidity,omitempty"`
	WindowOpen         *bool    `json:"window_open,omitempty"`
	Fault              string   `json:"fault,omitempty"`
	RSSI               *int     `json:"rssi,omitempty"`
	LastSeen           string   `json:"last_seen,omitempty"` // RFC 3339
	ControlMode        string   `json:"control_mode,omitempty"`
	BoostMinutes       *int     `json:"boost_minutes,omitempty"`
	BoostState         *int     `json:"boost_state,omitempty"`
	CommReporting      *bool    `json:"comm_reporting,omitempty"`
	AuthStatus         string   `json:"auth_status,omitempty"`
	MessagesReceived   *uint64  `json:"messages_received,omitempty"`
	WindowState        string   `json:"window_state,omitempty"`
	Sabotage           *bool    `json:"sabotage,omitempty"`
	LastCommand        string   `json:"last_command,omitempty"`
}

// MarshalJSON encodes the state with snake_case field names, omitting
// unknown fields.
func (s State) MarshalJSON() ([]byte, error) {
	j := stateJSON{
		CurrentTemperature: s.CurrentTemperature,
		TargetTemperature:  s.TargetTemperature,
		Mode:               s.Mode,
		Preset:             s.Preset,
		Action:             s.Action,
		ValvePosition:      s.ValvePosition,
		BatteryVoltage:     s.BatteryVoltage,
		LowBattery:         s.LowBattery,
		Humidity:           s.Humidity,
		WindowOpen:         s.WindowOpen,
		Fault:              s.Fault,
		RSSI:               s.RSSI,
		ControlMode:        s.ControlMode,
		BoostMinutes:       s.BoostMinutes,
		BoostState:         s.BoostState,
		CommReporting:      s.CommReporting,
		AuthStatus:         s.AuthStatus,
		MessagesReceived:   s.MessagesReceived,
		WindowState:        s.WindowState,
		Sabotage:           s.Sabotage,
		LastCommand:        s.LastCommand,
	}
	if !s.LastSeen.IsZero() {
		j.LastSeen = s.LastSeen.UTC().Format(time.RFC3339)
	}
	if s.BatteryVoltage != nil {
		j.BatteryPercent = ptr(BatteryPercent(*s.BatteryVoltage))
	}
	for _, f := range []*float64{j.CurrentTemperature, j.TargetTemperature, j.BatteryVoltage} {
		if f != nil && (math.IsNaN(*f) || math.IsInf(*f, 0)) {
			return nil, fmt.Errorf("ha: invalid number %v in state", *f)
		}
	}
	return json.Marshal(j)
}

// UnmarshalJSON decodes the format written by MarshalJSON (the derived
// battery_percent is ignored).
func (s *State) UnmarshalJSON(b []byte) error {
	var j stateJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	*s = State{
		CurrentTemperature: j.CurrentTemperature,
		TargetTemperature:  j.TargetTemperature,
		Mode:               j.Mode,
		Preset:             j.Preset,
		Action:             j.Action,
		ValvePosition:      j.ValvePosition,
		BatteryVoltage:     j.BatteryVoltage,
		LowBattery:         j.LowBattery,
		Humidity:           j.Humidity,
		WindowOpen:         j.WindowOpen,
		Fault:              j.Fault,
		RSSI:               j.RSSI,
		ControlMode:        j.ControlMode,
		BoostMinutes:       j.BoostMinutes,
		BoostState:         j.BoostState,
		CommReporting:      j.CommReporting,
		AuthStatus:         j.AuthStatus,
		MessagesReceived:   j.MessagesReceived,
		WindowState:        j.WindowState,
		Sabotage:           j.Sabotage,
		LastCommand:        j.LastCommand,
	}
	if j.LastSeen != "" {
		t, err := time.Parse(time.RFC3339, j.LastSeen)
		if err != nil {
			return err
		}
		s.LastSeen = t
	}
	return nil
}

// HAMode maps a HomeMatic control mode ("auto", "manual", "party",
// "boost") and target temperature to the HA climate mode and preset:
//
//	auto   → "auto", "none"
//	manual → "heat", "none"  ("off", "none" if target <= 4.5)
//	party  → "auto", "none"  (party is not exposed to HA)
//	boost  → "heat", "boost" (the device does not report the mode it
//	                          returns to after boost; while boosting the
//	                          valve is open, so "heat" is the honest mode)
//
// Unknown control modes return "", "" (no information; Merge keeps the
// previous values).
func HAMode(hmControlMode string, target float64) (mode, preset string) {
	switch strings.ToLower(hmControlMode) {
	case "auto", "party":
		return ModeAuto, PresetNone
	case "manual":
		if target <= MinTemp {
			return ModeOff, PresetNone
		}
		return ModeHeat, PresetNone
	case "boost":
		return ModeHeat, PresetBoost
	}
	return "", ""
}

// HAAction derives the HA action for a radiator thermostat from its
// valve position and mode.
func HAAction(mode string, valve int) string {
	if mode == ModeOff {
		return ActionOff
	}
	if valve > 0 {
		return ActionHeating
	}
	return ActionIdle
}

// TopicConfig holds the MQTT topic roots. Zero values mean the defaults.
type TopicConfig struct {
	Base            string // default "hmbridge"
	DiscoveryPrefix string // default "homeassistant"
}

const (
	DefaultBase            = "hmbridge"
	DefaultDiscoveryPrefix = "homeassistant"
)

func (c TopicConfig) base() string {
	if c.Base == "" {
		return DefaultBase
	}
	return strings.TrimSuffix(c.Base, "/")
}

func (c TopicConfig) prefix() string {
	if c.DiscoveryPrefix == "" {
		return DefaultDiscoveryPrefix
	}
	return strings.TrimSuffix(c.DiscoveryPrefix, "/")
}

// AvailabilityTopic is the bridge availability topic (LWT).
func (c TopicConfig) AvailabilityTopic() string { return c.base() + "/bridge/availability" }

// DeviceAvailabilityTopic is the retained availability topic of a single
// device ("online" while the device reports regularly, "offline" when it
// has been silent for too long). Device entities are available only when
// both the bridge and the device are online.
func (c TopicConfig) DeviceAvailabilityTopic(addr string) string {
	return c.base() + "/" + strings.ToLower(addr) + "/availability"
}

// DeviceAvailabilityMessage returns the retained availability message for
// the device at addr.
func DeviceAvailabilityMessage(cfg TopicConfig, addr string, online bool) Message {
	p := PayloadOffline
	if online {
		p = PayloadOnline
	}
	return Message{Topic: cfg.DeviceAvailabilityTopic(addr), Payload: []byte(p), Retain: true, QoS: 1}
}

// StateTopic is the retained JSON state topic of a device.
func (c TopicConfig) StateTopic(addr string) string {
	return c.base() + "/" + strings.ToLower(addr) + "/state"
}

// CommandTopic returns the command topic for what ("temperature", "mode",
// "preset").
func (c TopicConfig) CommandTopic(addr, what string) string {
	return c.base() + "/" + strings.ToLower(addr) + "/set/" + what
}

// GroupStateTopic is the retained JSON state topic of a heating group.
func (c TopicConfig) GroupStateTopic(slug string) string {
	return c.base() + "/group/" + slug + "/state"
}

// GroupCommandTopic returns a heating group's command topic for what.
func (c TopicConfig) GroupCommandTopic(slug, what string) string {
	return c.base() + "/group/" + slug + "/set/" + what
}

// CommandSubscription is the wildcard topic covering all device command
// topics.
func (c TopicConfig) CommandSubscription() string { return c.base() + "/+/set/+" }

// GroupCommandSubscription is the wildcard topic covering all group
// command topics.
func (c TopicConfig) GroupCommandSubscription() string { return c.base() + "/group/+/set/+" }

// HAStatusTopic is where HA publishes its birth ("online") message.
func (c TopicConfig) HAStatusTopic() string { return c.prefix() + "/status" }

// Message is an MQTT message to publish.
type Message struct {
	Topic   string
	Payload []byte
	Retain  bool
	QoS     byte
}

// StateMessage returns the retained state message for addr.
func StateMessage(cfg TopicConfig, addr string, s State) (Message, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return Message{}, err
	}
	return Message{Topic: cfg.StateTopic(addr), Payload: b, Retain: true, QoS: 1}, nil
}

// GroupStateMessage returns the retained state message for a heating
// group (same State/JSON format as devices; typically CurrentTemperature,
// TargetTemperature, Mode, Preset, Action, Humidity, WindowOpen).
func GroupStateMessage(cfg TopicConfig, slug string, s State) (Message, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return Message{}, err
	}
	return Message{Topic: cfg.GroupStateTopic(slug), Payload: b, Retain: true, QoS: 1}, nil
}

// CommandKind is the type of a Command.
type CommandKind int

const (
	SetTemperature CommandKind = iota + 1
	SetMode
	SetPreset
)

func (k CommandKind) String() string {
	switch k {
	case SetTemperature:
		return "SetTemperature"
	case SetMode:
		return "SetMode"
	case SetPreset:
		return "SetPreset"
	}
	return fmt.Sprintf("CommandKind(%d)", int(k))
}

// Command is a validated command from HA. Exactly one of Addr and Group
// is set.
type Command struct {
	Addr        string // device commands: lowercase hex address
	Group       string // group commands: group slug
	Kind        CommandKind
	Temperature float64 // SetTemperature: 4.5 … 30.5 in 0.5 steps
	Mode        string  // SetMode: "auto" | "heat" | "off"
	Preset      string  // SetPreset: "boost" | "none"
}

// ErrNotCommandTopic is returned by ParseCommand for topics that are not
// command topics.
var ErrNotCommandTopic = errors.New("ha: not a command topic")

// ParseCommand parses and validates a message received on a command topic.
// Temperatures are rounded to the nearest 0.5 °C and must lie within
// 4.5 … 30.5 °C.
func ParseCommand(cfg TopicConfig, topic string, payload []byte) (Command, error) {
	rest, ok := strings.CutPrefix(topic, cfg.base()+"/")
	if !ok {
		return Command{}, ErrNotCommandTopic
	}
	parts := strings.Split(rest, "/")
	var cmd Command
	var what string
	switch {
	case len(parts) == 4 && parts[0] == "group" && parts[2] == "set":
		if !ValidSlug(parts[1]) {
			return Command{}, fmt.Errorf("ha: invalid group slug %q in topic %q", parts[1], topic)
		}
		cmd.Group, what = parts[1], parts[3]
	case len(parts) == 3 && parts[1] == "set":
		addr := strings.ToLower(parts[0])
		if !validAddr(addr) {
			return Command{}, fmt.Errorf("ha: invalid device address %q in topic %q", parts[0], topic)
		}
		cmd.Addr, what = addr, parts[2]
	default:
		return Command{}, ErrNotCommandTopic
	}
	p := strings.TrimSpace(string(payload))
	switch what {
	case "temperature":
		t, err := strconv.ParseFloat(p, 64)
		if err != nil || math.IsNaN(t) || math.IsInf(t, 0) {
			return Command{}, fmt.Errorf("ha: invalid temperature %q", p)
		}
		t = math.Round(t/TempStep) * TempStep
		if t < MinTemp || t > MaxTemp {
			return Command{}, fmt.Errorf("ha: temperature %v out of range [%v, %v]", t, MinTemp, MaxTemp)
		}
		cmd.Kind, cmd.Temperature = SetTemperature, t
	case "mode":
		m := strings.ToLower(p)
		switch m {
		case ModeAuto, ModeHeat, ModeOff:
		default:
			return Command{}, fmt.Errorf("ha: invalid mode %q", p)
		}
		cmd.Kind, cmd.Mode = SetMode, m
	case "preset":
		m := strings.ToLower(p)
		switch m {
		case PresetBoost, PresetNone:
		default:
			return Command{}, fmt.Errorf("ha: invalid preset %q", p)
		}
		cmd.Kind, cmd.Preset = SetPreset, m
	default:
		return Command{}, fmt.Errorf("ha: unknown command %q", what)
	}
	return cmd, nil
}

func validAddr(a string) bool {
	if len(a) != 6 {
		return false
	}
	for _, c := range a {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
