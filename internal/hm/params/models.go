// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package params

import (
	"fmt"
	"sort"
	"strings"
)

// Register lists.
var (
	List0 = Loc{Channel: 0, List: 0} // device list
	// List7 is the thermostats' list 7 (accessed with channel 0, see
	// Loc).
	List7 = Loc{Channel: 0, List: 7}
)

// ChannelList1 returns list 1 of channel ch.
func ChannelList1(ch byte) Loc { return Loc{Channel: ch, List: 1} }

// Model is the settings description of one model.
type Model struct {
	Name     string
	Settings []Setting
	// Notes lists settings of the model that were deliberately left
	// out, with the reason (for the documentation and the tests).
	Notes []string
}

// Setting returns the setting with key, or nil.
func (m *Model) Setting(key string) *Setting {
	for i := range m.Settings {
		if m.Settings[i].Key == key {
			return &m.Settings[i]
		}
	}
	return nil
}

// Locs returns the register lists the model's settings live in, sorted.
func (m *Model) Locs() []Loc {
	seen := map[Loc]bool{}
	var res []Loc
	for _, s := range m.Settings {
		if !seen[s.Loc] {
			seen[s.Loc] = true
			res = append(res, s.Loc)
		}
	}
	sort.Slice(res, func(i, j int) bool {
		if res[i].Channel != res[j].Channel {
			return res[i].Channel < res[j].Channel
		}
		return res[i].List < res[j].List
	})
	return res
}

// ForModel returns the settings description of model (nil if hmbridge
// exposes no settings for it).
func ForModel(model string) *Model {
	for _, m := range models {
		if strings.EqualFold(m.Name, model) {
			return m
		}
	}
	return nil
}

// Catalog returns every setting key of every model once (for removing
// the Home Assistant entities a device no longer has), sorted by key.
func Catalog() []Setting {
	seen := map[string]bool{}
	var res []Setting
	for _, m := range models {
		for _, s := range m.Settings {
			if !seen[s.Key] {
				seen[s.Key] = true
				res = append(res, s)
			}
		}
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Key < res[j].Key })
	return res
}

// Source abbreviations.
const (
	srcRT  = "eQ-3 rf_cc_rt_dn.xml (as shipped with Homegear)"
	srcTC  = "eQ-3 rf_tc_it_wm-w-eu.xml (as shipped with Homegear)"
	srcSCo = "eQ-3 rf_sec_sco.xml (as shipped with Homegear)"
	srcPB  = "eQ-3 rf_pb-2-wm55_le_v1_3.xml / _ge_v1_4.xml and rf_rc-4-2.xml (as shipped with Homegear)"
	srcSw  = "eQ-3 rf_s_644.xml (as shipped with Homegear)"
	srcDim = "eQ-3 rf_dim_1l_644.xml (as shipped with Homegear)"
	srcLED = "eQ-3 rf_ou_led16_le_v1_0.xml / _ge_v1_1.xml (as shipped with Homegear)"
	fhem   = "; FHEM HMConfig.pm "
)

func src(xml, param, pos, fhemReg string) string {
	return fmt.Sprintf("%s %s %s%s%s", xml, param, pos, fhem, fhemReg)
}

// thermostatSettings are the settings both thermostats share (same
// registers in both descriptions).
func thermostatSettings(xml string) []Setting {
	temp := func(key, name, param string, idx byte, min, max float64, fhemReg string) Setting {
		return Setting{Key: key, Name: name, Kind: Number, Loc: List7, Field: Bits(idx, 0, 6),
			Min: min, Max: max, Step: 0.5, Scale: 2, Unit: "°C", Enabled: true, Icon: "mdi:thermometer",
			Source: src(xml, param, fmt.Sprintf("list 7 index %d.0 size 0.6, °C·2", idx), fhemReg)}
	}
	return []Setting{
		temp("comfort_temperature", "Comfort temperature", "TEMPERATURE_COMFORT", 1, 15, 30, "dayTemp"),
		temp("eco_temperature", "Eco temperature", "TEMPERATURE_LOWERING", 2, 5, 25, "nightTemp"),
		temp("min_temperature", "Minimum temperature", "TEMPERATURE_MINIMUM", 3, 4.5, 14.5, "tempMin"),
		temp("max_temperature", "Maximum temperature", "TEMPERATURE_MAXIMUM", 4, 15, 30.5, "tempMax"),
		{Key: "temperature_offset", Name: "Temperature offset", Kind: Number, Loc: List7, Field: Bits(9, 0, 4),
			// Enumeration -3.5K (0) … 0.0K (7) … 3.5K (14).
			Min: -3.5, Max: 3.5, Step: 0.5, Offset: -3.5, Scale: 2, Unit: "K", Enabled: true, Icon: "mdi:thermometer-plus",
			Source: src(xml, "TEMPERATURE_OFFSET", "list 7 index 9.0 size 0.4, enum -3.5K=0 … 3.5K=14", "tempOffset")},
		{Key: "boost_duration", Name: "Boost duration", Kind: Number, Loc: List7, Field: Bits(10, 5, 3),
			// Enumeration 0 min (0) … 30 min (6).
			Min: 0, Max: 30, Step: 5, Scale: 0.2, Unit: "min", Enabled: true, Icon: "mdi:rocket-launch",
			Source: src(xml, "BOOST_TIME_PERIOD", "list 7 index 10.5 size 0.3, enum 0 min=0 … 30 min=6", "boostPeriod")},
		{Key: "button_lock", Name: "Button lock", Kind: Switch, Loc: List0, Field: Byte(15), Enabled: true, Icon: "mdi:lock",
			Source: src(xml, "BUTTON_LOCK", "list 0 index 15.0 size 1.0, boolean", "btnLock (on=1, “1 is proofen”)")},
		{Key: "daylight_saving_time", Name: "Daylight saving time", Kind: Switch, Loc: List7, Field: Bits(14, 0, 1), Icon: "mdi:clock-time-four-outline",
			Source: src(xml, "DAYLIGHT_SAVING_TIME", "list 7 index 14.0 size 0.1", "daylightSaveTime")},
		{Key: "boost_after_window_open", Name: "Boost after window open", Kind: Switch, Loc: List7, Field: Bits(14, 5, 1), Icon: "mdi:rocket-launch-outline",
			Source: src(xml, "BOOST_AFTER_WINDOW_OPEN", "list 7 index 14.5 size 0.1", "winOpnBoost")},
		{Key: "display_information", Name: "Display time or date", Kind: Select, Loc: List7, Field: Bits(14, 3, 2),
			Options: []Option{{"time", 0}, {"date", 1}}, Icon: "mdi:calendar-clock",
			Source: src(xml, "DISPLAY_INFORMATION", "list 7 index 14.3 size 0.2, TIME=0 DATE=1", "showInfo")},
		{Key: "low_battery_limit", Name: "Low battery limit", Kind: Number, Loc: List0, Field: Byte(18),
			Min: 2.0, Max: 2.5, Step: 0.1, Scale: 10, Unit: "V", Icon: "mdi:battery-alert",
			Source: src(xml, "LOW_BAT_LIMIT", "list 0 index 18.0 size 1.0, V·10", "lowBatLimitRT")},
	}
}

// weekProfile returns the 7 week profile settings (Monday first).
func weekProfile(xml string) []Setting {
	var res []Setting
	for _, day := range []int{2, 3, 4, 5, 6, 0, 1} {
		name := RegisterDays[day]
		res = append(res, Setting{Key: "week_profile_" + name, Name: "Week profile " + strings.ToUpper(name[:1]) + name[1:],
			Kind: Profile, Loc: List7, Day: day, ProfileBase: ProfileBase, Enabled: true, Icon: "mdi:calendar-week",
			Source: src(xml, "ENDTIME_/TEMPERATURE_"+strings.ToUpper(name)+"_1…13",
				fmt.Sprintf("list 7 index %d.0 + 2·(n-1), size 2.0 mask 511 (min/5) and .1 size 0.6 (°C·2)", ProfileBase+day*dayRegs),
				"tempList"+strings.ToUpper(name[:1])+name[1:3])})
	}
	return res
}

var weekdayOptions = []Option{
	{"Saturday", 0}, {"Sunday", 1}, {"Monday", 2}, {"Tuesday", 3}, {"Wednesday", 4}, {"Thursday", 5}, {"Friday", 6},
}

// halfHours are the options 00:00 … 23:30 (raw = minutes / 30).
func halfHours() []Option {
	var o []Option
	for i := 0; i < 48; i++ {
		o = append(o, Option{fmt.Sprintf("%02d:%02d", i/2, i%2*30), uint32(i)})
	}
	return o
}

func rtSettings() []Setting {
	s := thermostatSettings(srcRT)
	s = append(s,
		Setting{Key: "window_open_temperature", Name: "Window open temperature", Kind: Number, Loc: List7, Field: Bits(5, 0, 6),
			Min: 5, Max: 30, Step: 0.5, Scale: 2, Unit: "°C", Enabled: true, Icon: "mdi:window-open-variant",
			Source: src(srcRT, "TEMPERATUREFALL_WINDOW_OPEN", "list 7 index 5.0 size 0.6, °C·2", "winOpnTemp")},
		Setting{Key: "window_open_duration", Name: "Window open duration", Kind: Number, Loc: List7, Field: Bits(6, 0, 4),
			Min: 0, Max: 60, Step: 5, Scale: 0.2, Unit: "min", Enabled: true, Icon: "mdi:timer-outline",
			Source: src(srcRT, "TEMPERATUREFALL_WINDOW_OPEN_TIME_PERIOD", "list 7 index 6.0 size 0.4, min/5", "winOpnPeriod")},
		Setting{Key: "window_detection_mode", Name: "Window open detection", Kind: Select, Loc: List7, Field: Bits(19, 5, 3),
			Options: []Option{{"off", 0}, {"auto mode", 1}, {"auto and manual mode", 2}, {"auto and party mode", 3}, {"on", 4}},
			Icon:    "mdi:window-open-variant",
			Source:  src(srcRT, "TEMPERATUREFALL_MODUS", "list 7 index 19.5 size 0.3, DEAKTIV=0 … AKTIV=4", "winOpnMode")},
		Setting{Key: "decalcification_day", Name: "Decalcification day", Kind: Select, Loc: List7, Field: Bits(7, 0, 3),
			Options: weekdayOptions, Icon: "mdi:calendar",
			Source: src(srcRT, "DECALCIFICATION_WEEKDAY", "list 7 index 7.0 size 0.3, SATURDAY=0 … FRIDAY=6", "decalcWeekday")},
		Setting{Key: "decalcification_time", Name: "Decalcification time", Kind: Select, Loc: List7, Field: Bits(8, 0, 6),
			Options: halfHours(), Icon: "mdi:clock-outline",
			Source: src(srcRT, "DECALCIFICATION_TIME", "list 7 index 8.0 size 0.6, minutes/30, 0 … 1410", "decalcTime")},
		Setting{Key: "boost_valve_position", Name: "Boost valve position", Kind: Number, Loc: List7, Field: Bits(10, 0, 5),
			Min: 0, Max: 100, Step: 5, Scale: 0.2, Unit: "%", Icon: "mdi:pipe-valve",
			Source: src(srcRT, "BOOST_POSITION", "list 7 index 10.0 size 0.5, %/5", "boostPos")},
		Setting{Key: "valve_offset", Name: "Valve offset", Kind: Number, Loc: List7, Field: Bits(11, 0, 7),
			Min: 0, Max: 100, Step: 1, Unit: "%", Icon: "mdi:pipe-valve",
			Source: src(srcRT, "VALVE_OFFSET", "list 7 index 11.0 size 0.7", "valveOffsetRt")},
		Setting{Key: "valve_max_position", Name: "Maximum valve position", Kind: Number, Loc: List7, Field: Bits(12, 0, 7),
			Min: 0, Max: 100, Step: 1, Unit: "%", Icon: "mdi:pipe-valve",
			Source: src(srcRT, "VALVE_MAXIMUM_POSITION", "list 7 index 12.0 size 0.7", "valveMaxPos")},
		Setting{Key: "adaptive_regulation", Name: "Adaptive regulation", Kind: Select, Loc: List7, Field: Bits(14, 1, 2),
			Options: []Option{{"off (default values)", 0}, {"off (learned values)", 1}, {"on", 2}}, Icon: "mdi:tune-variant",
			Source: src(srcRT, "ADAPTIVE_REGULATION", "list 7 index 14.1 size 0.2", "regAdaptive")},
		Setting{Key: "backlight_on_time", Name: "Backlight on time", Kind: Number, Loc: List0, Field: Byte(14),
			Min: 0, Max: 255, Step: 1, Unit: "s", Icon: "mdi:brightness-6",
			Source: src(srcRT, "BACKLIGHT_ON_TIME", "list 0 index 14.0 size 1.0", "backOnTime")},
	)
	return append(s, weekProfile(srcRT)...)
}

func tcSettings() []Setting {
	s := thermostatSettings(srcTC)
	s = append(s,
		Setting{Key: "display_temperature", Name: "Display temperature", Kind: Select, Loc: List7, Field: Bits(9, 5, 1),
			Options: []Option{{"actual temperature", 0}, {"set temperature", 1}}, Enabled: true, Icon: "mdi:thermometer",
			Source: src(srcTC, "SHOW_SET_TEMPERATUR", "list 7 index 9.5 size 0.1, ACTUAL=0 SET=1", "showSetTemp")},
		Setting{Key: "display_humidity", Name: "Display humidity", Kind: Select, Loc: List7, Field: Bits(9, 6, 1),
			Options: []Option{{"temperature only", 0}, {"temperature and humidity", 1}}, Enabled: true, Icon: "mdi:water-percent",
			Source: src(srcTC, "SHOW_HUMIDITY", "list 7 index 9.6 size 0.1", "showHumidity")},
		Setting{Key: "heating_cooling", Name: "Heating or cooling", Kind: Select, Loc: List7, Field: Bits(15, 7, 1),
			Options: []Option{{"heating", 0}, {"cooling", 1}}, Icon: "mdi:sun-snowflake-variant",
			Source: src(srcTC, "HEATING_COOLING", "list 7 index 15.7 size 0.1", "heatCool")},
	)
	return append(s, weekProfile(srcTC)...)
}

func buttonSettings(buttons int) []Setting {
	var s []Setting
	for n := 1; n <= buttons; n++ {
		loc := ChannelList1(byte(n))
		s = append(s,
			Setting{Key: fmt.Sprintf("long_press_time_%d", n), Name: fmt.Sprintf("Button %d long press time", n), Kind: Number,
				Loc: loc, Field: Bits(4, 4, 4), Min: 0.3, Max: 1.8, Step: 0.1, Offset: 0.3, Scale: 10, Unit: "s", Icon: "mdi:timer-outline",
				Source: src(srcPB, "LONG_PRESS_TIME", "list 1 index 4.4 size 0.4, (s-0.3)·10", "longPress (m10s3)")},
			Setting{Key: fmt.Sprintf("double_press_time_%d", n), Name: fmt.Sprintf("Button %d double press time", n), Kind: Number,
				Loc: loc, Field: Bits(9, 0, 4), Min: 0, Max: 1.5, Step: 0.1, Scale: 10, Unit: "s", Icon: "mdi:timer-outline",
				Source: src(srcPB, "DBL_PRESS_TIME", "list 1 index 9.0 size 0.4, s·10", "dblPress")},
		)
	}
	return s
}

func powerOnAction(xml string) Setting {
	return Setting{Key: "power_on_action", Name: "Power-on action", Kind: Select, Loc: ChannelList1(1), Field: Bits(86, 0, 1),
		// eQ-3: POWERUP_OFF / POWERUP_ON; FHEM: “on: simulate short
		// press of peer self01 after power up” (the internal button's
		// link decides what happens).
		Options: []Option{{"off", 0}, {"on", 1}}, Enabled: true, Icon: "mdi:power-plug-outline",
		Source: src(xml, "POWERUP_ACTION", "list 1 index 86.0 size 0.1", "powerUpAction")}
}

// Notes on settings left out (sources disagree, or the setting belongs
// to a direct link, which is out of scope).
const (
	noteGlobalLock = "GLOBAL_BUTTON_LOCK / MODUS_BUTTON_LOCK (list 0 index 25/26): eQ-3/Homegear write 1, FHEM writes 200 for “on”; not exposed until verified on a device"
	noteResetDis   = "LOCAL_RESET_DISABLE (list 0 index 24): Homegear bit 0, FHEM the whole register with 200; not exposed"
	noteRTDisplay  = "the radiator thermostat has no display mode setting (SHOW_SET_TEMPERATUR exists on the wall thermostat only)"
	noteTCWindow   = "the wall thermostat has no window open temperature/duration and no decalcification settings in its MASTER set"
	noteTCPrograms = "week programs 2 and 3 (lists 8 and 9) and WEEK_PROGRAM_POINTER are not exposed; the week profile entities edit program 1"
	noteDimLinks   = "ramp times, on level and minimum/maximum level are link parameters (list 3), not MASTER settings: out of scope"
	noteNoLED      = "no LED / status indication setting in the MASTER set"
)

var models = []*Model{
	{Name: "HM-CC-RT-DN", Settings: rtSettings(), Notes: []string{noteGlobalLock, noteResetDis, noteRTDisplay}},
	{Name: "HM-TC-IT-WM-W-EU", Settings: tcSettings(), Notes: []string{noteGlobalLock, noteResetDis, noteTCWindow, noteTCPrograms}},
	{Name: "HM-Sec-SCo", Settings: []Setting{
		{Key: "event_delay", Name: "Event delay", Kind: Number, Loc: ChannelList1(1), Field: Byte(33),
			Min: 0, Max: 7620, Step: 1, ConfigTime60: true, Unit: "s", Enabled: true, Icon: "mdi:timer-sand",
			Source: src(srcSCo, "EVENT_DELAYTIME", "list 1 index 33.0 size 1.0, decimalConfigTime value size 0.7, factors 1 and 60", "eventDlyTime (fltCvT60)")},
		{Key: "cyclic_info_message", Name: "Cyclic status messages", Kind: Switch, Loc: List0, Field: Byte(9), Enabled: true, Icon: "mdi:message-badge-outline",
			Source: src(srcSCo, "CYCLIC_INFO_MSG", "list 0 index 9.0 size 1.0, boolean", "cyclicInfoMsg (on=1)")},
		{Key: "sabotage_message", Name: "Sabotage message", Kind: Switch, Loc: List0, Field: Byte(16), Icon: "mdi:alert-outline",
			Source: src(srcSCo, "SABOTAGE_MSG", "list 0 index 16.0 size 1.0, boolean", "sabotageMsg (on=1)")},
	}, Notes: []string{noteResetDis, noteNoLED, "MSG_FOR_POS_A/B: Homegear and FHEM swap index 32.4 and 32.6; not exposed"}},
	{Name: "HM-PB-2-WM55", Settings: buttonSettings(2), Notes: []string{noteNoLED}},
	{Name: "HM-RC-8", Settings: buttonSettings(8), Notes: []string{noteNoLED, noteResetDis}},
	{Name: "HM-LC-Sw1-Pl-DN-R1", Settings: []Setting{powerOnAction(srcSw)},
		Notes: []string{noteNoLED, noteResetDis, "STATUSINFO_MINDELAY/RANDOM and TRANSMIT_TRY_MAX are not exposed (technical)"}},
	{Name: "HM-LC-Dim1L-Pl-3", Settings: []Setting{
		powerOnAction(srcDim),
		{Key: "load_appear_behaviour", Name: "Behaviour when the load reappears", Kind: Select, Loc: ChannelList1(1), Field: Bits(49, 0, 2),
			Options: []Option{{"off", 0}, {"last level", 1}, {"button press", 2}, {"button press if it was on", 3}}, Icon: "mdi:lightbulb-auto-outline",
			Source: src(srcDim, "LOAD_APPEAR_BEHAVIOUR", "list 1 index 49.0 size 0.2, RESTART_OFF=0 … RESTART_BUTTONPRESS_IF_WAS_ON=3", "loadAppearBehav")},
	}, Notes: []string{noteDimLinks, noteNoLED, noteResetDis}},
	{Name: "HM-OU-LED16", Settings: []Setting{
		{Key: "display_brightness", Name: "Display brightness", Kind: Number, Loc: List0, Field: Bits(4, 0, 4),
			Min: 0, Max: 15, Step: 1, Enabled: true, Icon: "mdi:brightness-6",
			Source: src(srcLED, "DISPLAY_BRIGHTNESS", "list 0 index 4.0 size 0.4, 0 … 15", "brightness")},
	}, Notes: []string{"DISPLAY_ENERGYOPTIONS (list 0 index 8): Homegear 0 … 255 s, FHEM 0 … 127 s with 0 = permanent; not exposed"}},
}
