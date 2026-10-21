package bridge

import (
	"fmt"
	"math"

	"github.com/stapelberg/hmgo/internal/ha"
	"github.com/stapelberg/hmgo/internal/hm"
	"github.com/stapelberg/hmgo/internal/hm/contact"
	"github.com/stapelberg/hmgo/internal/hm/heating"
	"github.com/stapelberg/hmgo/internal/hm/thermal"
)

// stateFromEvent converts a decoded device event into a (partial) Home
// Assistant state update. Unknown or irrelevant events yield an empty
// update.
func stateFromEvent(d *device, ev any) ha.State {
	var s ha.State
	switch e := ev.(type) {

	// HM-CC-RT-DN
	case *heating.InfoEvent:
		s.CurrentTemperature = ptr(e.ActualTemperature)
		s.TargetTemperature = ptr(e.SetTemperature)
		s.ValvePosition = ptr(int(e.ValveState))
		s.BatteryVoltage = ptr(round1(e.BatteryState))
		s.Fault = e.Fault.String()
		// The RT-DN reports low battery as a fault value, so another
		// fault hides it; only update the flag when it is unambiguous.
		if e.Fault == heating.NoFault || e.Fault == heating.Lowbat {
			s.LowBattery = ptr(e.Fault == heating.Lowbat)
		}
		setControl(&s, e.Control.String(), e.SetTemperature, int(e.BoostState), true)

	case *heating.AckStatusEvent:
		s.TargetTemperature = ptr(e.SetTemperature)
		s.LowBattery = ptr(e.LowBattery)
		s.Fault = e.Fault.String()
		if e.HasControl {
			setControl(&s, e.Control.String(), e.SetTemperature, int(e.BoostState), true)
		}

	// HM-TC-IT-WM-W-EU
	case *thermal.InfoEvent:
		s.CurrentTemperature = ptr(e.ActualTemperature)
		s.TargetTemperature = ptr(e.SetTemperature)
		s.LowBattery = ptr(e.LowbatReporting)
		s.CommReporting = ptr(e.CommunicationReporting)
		s.WindowOpen = ptr(e.WindowOpenReporting)
		s.BatteryVoltage = ptr(round1(e.BatteryState))
		setControl(&s, e.Control.String(), e.SetTemperature, int(e.BoostState), false)

	case *thermal.AckStatusEvent:
		s.TargetTemperature = ptr(e.SetTemperature)
		s.LowBattery = ptr(e.LowbatReporting)
		s.CommReporting = ptr(e.CommunicationReporting)
		s.WindowOpen = ptr(e.WindowOpenReporting)
		if e.HasControl {
			setControl(&s, e.Control.String(), e.SetTemperature, int(e.BoostState), false)
		}

	case *thermal.ThermalControlEvent:
		s.CurrentTemperature = ptr(e.ActualTemperature)
		s.TargetTemperature = ptr(e.SetTemperature)
		s.Humidity = ptr(int(math.Round(e.ActualHumidity)))

	case *thermal.WeatherEvent:
		s.CurrentTemperature = ptr(e.Temperature)
		s.Humidity = ptr(int(e.Humidity))

	case *thermal.SwitchEvent:
		s.LowBattery = ptr(e.LowBattery)

	// window contacts
	case *contact.Event:
		switch e.State {
		case contact.Closed:
			s.WindowState = ha.WindowClosed
			s.WindowOpen = ptr(false)
		case contact.Tilted:
			s.WindowState = ha.WindowTilted
			s.WindowOpen = ptr(true)
		case contact.Open:
			s.WindowState = ha.WindowOpened
			s.WindowOpen = ptr(true)
		}
		s.LowBattery = ptr(e.LowBattery)
		if e.HasError {
			s.Sabotage = ptr(e.Sabotage)
		}
	}
	return s
}

// setControl fills the control-mode related fields.
func setControl(s *ha.State, controlMode string, target float64, boost int, boostIsMinutes bool) {
	s.ControlMode = controlMode
	if controlMode == "boost" {
		if boostIsMinutes {
			s.BoostMinutes = ptr(boost)
		} else {
			s.BoostState = ptr(boost)
		}
	} else if boostIsMinutes {
		s.BoostMinutes = ptr(0)
	} else {
		s.BoostState = ptr(0)
	}
	mode, preset := ha.HAMode(controlMode, target)
	s.Mode = mode
	s.Preset = preset
}

// deriveState computes fields derived from other fields.
func deriveState(d *device) {
	if d.desc.Kind == ha.RadiatorThermostat && d.state.Mode != "" {
		valve := 0
		if d.state.ValvePosition != nil {
			valve = *d.state.ValvePosition
		}
		d.state.Action = ha.HAAction(d.state.Mode, valve)
	}
}

// groupState aggregates the state of a heating group from its members.
// Temperatures, target and mode come from the group's climate device
// (wall thermostat if present); window and low battery are “any member”.
func groupState(g *group) ha.State {
	var s ha.State
	if c := g.climate; c != nil {
		s.CurrentTemperature = c.state.CurrentTemperature
		s.TargetTemperature = c.state.TargetTemperature
		s.Humidity = c.state.Humidity
		s.Mode = c.state.Mode
		s.Preset = c.state.Preset
		s.ControlMode = c.state.ControlMode
		s.BoostMinutes = c.state.BoostMinutes
		s.BoostState = c.state.BoostState
	}

	var anyWindowKnown, windowOpen, anyBatKnown, lowBat bool
	maxValve := -1
	fault := ""
	for _, d := range append(append([]*device(nil), g.thermostats...), g.contacts...) {
		st := &d.state
		if st.WindowOpen != nil {
			anyWindowKnown = true
			windowOpen = windowOpen || *st.WindowOpen
		}
		if st.LowBattery != nil {
			anyBatKnown = true
			lowBat = lowBat || *st.LowBattery
		}
		if st.ValvePosition != nil && *st.ValvePosition > maxValve {
			maxValve = *st.ValvePosition
		}
		if st.Fault != "" && st.Fault != ha.FaultNone && fault == "" {
			fault = fmt.Sprintf("%s: %s", d.cfg.Name, st.Fault)
		}
		if st.LastSeen.After(s.LastSeen) {
			s.LastSeen = st.LastSeen
		}
	}
	if anyWindowKnown {
		s.WindowOpen = ptr(windowOpen)
		if windowOpen {
			s.WindowState = ha.WindowOpened
		} else {
			s.WindowState = ha.WindowClosed
		}
	}
	if anyBatKnown {
		s.LowBattery = ptr(lowBat)
	}
	if maxValve >= 0 {
		s.ValvePosition = ptr(maxValve)
		if s.Mode != "" {
			s.Action = ha.HAAction(s.Mode, maxValve)
		}
	}
	if fault == "" && len(g.thermostats) > 0 {
		fault = ha.FaultNone
	}
	s.Fault = fault
	return s
}

func eventName(ev any) string {
	switch e := ev.(type) {
	case *heating.InfoEvent, *thermal.InfoEvent:
		return "info"
	case *heating.AckStatusEvent, *thermal.AckStatusEvent:
		return "ack-status"
	case *heating.TeamEvent:
		return "team"
	case *thermal.ThermalControlEvent:
		return "thermal-control"
	case *thermal.WeatherEvent:
		return "weather"
	case *thermal.SwitchEvent:
		return "switch"
	case *contact.Event:
		return "contact-" + e.State.String()
	case *hm.TimeRequest:
		return "time-request"
	case *hm.DeviceInfo:
		return "device-info"
	}
	return fmt.Sprintf("%T", ev)
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
