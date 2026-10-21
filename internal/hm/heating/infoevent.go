package heating

import (
	"bytes"
	"fmt"
	"html/template"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stapelberg/hmgo/internal/bidcos"
	"github.com/stapelberg/hmgo/internal/hm"
)

const prometheusNamespace = "hmheating"

var (
	infoEventSetTemperature = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "InfoEventSetTemperature",
			Help:      "target temperature in degC",
		},
		[]string{"address", "name"})

	infoEventActualTemperature = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "InfoEventActualTemperature",
			Help:      "current temperature in degC",
		},
		[]string{"address", "name"})

	infoEventFault = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "InfoEventFault",
			Help:      "fault as bool",
		},
		[]string{"address", "name", "fault"})

	infoEventBatteryState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "InfoEventBatteryState",
			Help:      "battery state in V",
		},
		[]string{"address", "name", "hmtype"})

	infoEventValveState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "InfoEventValveState",
			Help:      "valve state in percentage points",
		},
		[]string{"address", "name"})

	infoEventControl = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "InfoEventControl",
			Help:      "control mode",
		},
		[]string{"address", "name", "mode"})

	infoEventBoostState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "InfoEventBoostState",
			Help:      "boost state in minutes",
		},
		[]string{"address", "name"})
)

func init() {
	prometheus.MustRegister(infoEventSetTemperature)
	prometheus.MustRegister(infoEventActualTemperature)
	prometheus.MustRegister(infoEventFault)
	prometheus.MustRegister(infoEventBatteryState)
	prometheus.MustRegister(infoEventValveState)
	prometheus.MustRegister(infoEventControl)
	prometheus.MustRegister(infoEventBoostState)
}

// ControlMode is the thermostat's operating mode.
type ControlMode uint

const (
	AutoMode ControlMode = iota
	ManuMode
	PartyMode
	BoostMode
)

func (cm ControlMode) String() string {
	switch cm {
	case AutoMode:
		return "auto"
	case ManuMode:
		return "manual"
	case PartyMode:
		return "party"
	case BoostMode:
		return "boost"
	default:
		return fmt.Sprintf("unknown mode (%d)", uint(cm))
	}
}

// FaultReporting is the RT-DN's FAULT_REPORTING value.
type FaultReporting uint

const (
	NoFault FaultReporting = iota
	ValveTight
	AdjustingRangeTooLarge
	AdjustingRangeTooSmall
	CommunicationError
	_
	Lowbat
	ValveErrorPosition
)

func (fr FaultReporting) String() string {
	switch fr {
	case NoFault:
		return "none"
	case ValveTight:
		return "valve tight"
	case AdjustingRangeTooLarge:
		return "adjusting range too large"
	case AdjustingRangeTooSmall:
		return "adjusting range too small"
	case CommunicationError:
		return "communication error"
	case Lowbat:
		return "low battery"
	case ValveErrorPosition:
		return "valve error position"
	default:
		return fmt.Sprintf("unknown fault (%d dec, %x hex)", uint(fr), uint(fr))
	}
}

// InfoEvent is the RT-DN's INFO_LEVEL frame (cmd 0x10, subtype 0x0A),
// reporting channel 4. Payload layout (byte offsets in the payload,
// i.e. rftypes index - 9):
//
//	[0]    0x0A
//	[1..2] SET_TEMPERATURE = [1]>>2 (×0.5 °C),
//	       ACTUAL_TEMPERATURE = ([1]&3)<<8 | [2] (×0.1 °C)
//	[3]    FAULT_REPORTING = [3]>>5, BATTERY_STATE = ([3]&0x1f)/10+1.5 V
//	[4]    VALVE_STATE = [4]&0x7f (%)
//	[5]    CONTROL_MODE = [5]>>6, BOOST_STATE = [5]&0x3f (minutes, in boost mode)
//	[6..]  party mode start/stop/temperature (only in party mode; ignored)
type InfoEvent struct {
	SetTemperature    float64 // in degC; 4.5 = off, 30.5 = on
	ActualTemperature float64 // in degC
	Fault             FaultReporting
	BatteryState      float64 // in V
	ValveState        uint64  // in percentage points
	Control           ControlMode
	BoostState        uint64 // remaining boost time in minutes (only meaningful in BoostMode)
	// Not using party mode, so ignoring the remaining fields.
}

// LowBattery reports whether the device reports a low battery. The
// RT-DN signals this through FAULT_REPORTING (6 = lowbat) only. Note
// that a different fault hides the low battery state.
func (ie *InfoEvent) LowBattery() bool { return ie.Fault == Lowbat }

var ieTmpl = template.Must(template.New("infoevent").Parse(`
<strong>Info:</strong><br>
Target temperature: {{ .SetTemperature }} ℃<br>
Current temperature: {{ .ActualTemperature }} ℃<br>
Fault: {{ .Fault }}<br>
Battery state: {{ .BatteryState }} V<br>
Valve state: {{ .ValveState }}%<br>
Control: {{ .Control }}<br>
Boost state: {{ .BoostState }} minutes<br>
`))

func (ie *InfoEvent) HTML() template.HTML {
	var buf bytes.Buffer
	if err := ieTmpl.Execute(&buf, ie); err != nil {
		return template.HTML(template.HTMLEscapeString(err.Error()))
	}
	return template.HTML(buf.String())
}

func (t *Thermostat) DecodeInfoEvent(payload []byte) (*InfoEvent, error) {
	// c.f. <frame id="INFO_LEVEL"> in rftypes/rf_cc_rt_dn.xml. Frames in
	// party mode carry 6 more bytes, which we ignore.
	if got, want := len(payload), 6; got < want {
		return nil, fmt.Errorf("unexpected payload size: got %d, want >= %d", got, want)
	}
	if payload[0] != bidcos.InfoTemp {
		return nil, fmt.Errorf("not an INFO_LEVEL frame: subtype 0x%02x", payload[0])
	}
	ie := &InfoEvent{
		SetTemperature:    float64((uint64(payload[1])>>2)&hm.Mask6Bit) / 2,
		ActualTemperature: float64(((int64(payload[1])&hm.Mask2Bit)<<8)|int64(payload[2])) / 10,
		Fault:             FaultReporting((payload[3] >> 5) & hm.Mask3Bit),
		BatteryState:      float64(payload[3]&hm.Mask5Bit)/10 + 1.5,
		ValveState:        uint64(payload[4] & hm.Mask7Bit),
		Control:           ControlMode((payload[5] >> 6) & hm.Mask2Bit),
		BoostState:        uint64(payload[5] & hm.Mask6Bit),
	}

	infoEventSetTemperature.With(prometheus.Labels{"name": t.Name(), "address": t.AddrHex()}).Set(ie.SetTemperature)
	infoEventActualTemperature.With(prometheus.Labels{"name": t.Name(), "address": t.AddrHex()}).Set(ie.ActualTemperature)

	infoEventFault.With(prometheus.Labels{"name": t.Name(), "address": t.AddrHex(), "fault": "valvetight"}).Set(boolToFloat64(ie.Fault == ValveTight))
	infoEventFault.With(prometheus.Labels{"name": t.Name(), "address": t.AddrHex(), "fault": "adjustingrangetoosmall"}).Set(boolToFloat64(ie.Fault == AdjustingRangeTooSmall))
	infoEventFault.With(prometheus.Labels{"name": t.Name(), "address": t.AddrHex(), "fault": "adjustingrangetoolarge"}).Set(boolToFloat64(ie.Fault == AdjustingRangeTooLarge))
	infoEventFault.With(prometheus.Labels{"name": t.Name(), "address": t.AddrHex(), "fault": "communicationerror"}).Set(boolToFloat64(ie.Fault == CommunicationError))
	infoEventFault.With(prometheus.Labels{"name": t.Name(), "address": t.AddrHex(), "fault": "lowbat"}).Set(boolToFloat64(ie.Fault == Lowbat))
	infoEventFault.With(prometheus.Labels{"name": t.Name(), "address": t.AddrHex(), "fault": "valveerrorposition"}).Set(boolToFloat64(ie.Fault == ValveErrorPosition))

	infoEventBatteryState.With(prometheus.Labels{"name": t.Name(), "address": t.AddrHex(), "hmtype": "heating"}).Set(ie.BatteryState)
	infoEventValveState.With(prometheus.Labels{"name": t.Name(), "address": t.AddrHex()}).Set(float64(ie.ValveState))

	infoEventControl.With(prometheus.Labels{"name": t.Name(), "address": t.AddrHex(), "mode": "manu"}).Set(boolToFloat64(ie.Control == ManuMode))
	infoEventControl.With(prometheus.Labels{"name": t.Name(), "address": t.AddrHex(), "mode": "party"}).Set(boolToFloat64(ie.Control == PartyMode))
	infoEventControl.With(prometheus.Labels{"name": t.Name(), "address": t.AddrHex(), "mode": "boost"}).Set(boolToFloat64(ie.Control == BoostMode))

	infoEventBoostState.With(prometheus.Labels{"name": t.Name(), "address": t.AddrHex()}).Set(float64(ie.BoostState))

	t.latestMu.Lock()
	defer t.latestMu.Unlock()
	t.latestInfoEvent = ie
	return ie, nil
}

func boolToFloat64(val bool) float64 {
	var converted float64
	if val {
		converted = 1
	}
	return converted
}
