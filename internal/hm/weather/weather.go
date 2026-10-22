// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

// Package weather implements the battery powered outdoor sensors:
//
//	HM-WDS10-TH-O  temperature and humidity  (rftypes rf_ash550.xml)
//	HM-WDS30-T-O   temperature only          (rftypes rf_s550ia.xml)
//
// Both have one channel, no AES, and only send: a WEATHER_EVENT (BidCoS
// cmd 0x70) broadcast every few minutes (flags 0x86 WAKEMEUP|BCAST|RPTEN
// in the user's captures), payload:
//
//	[0] bit 7    low battery (FHEM CUL_HM; eQ-3 derives LOWBAT internally)
//	[0] bits 0-6, [1]  TEMPERATURE, 15 bit two's complement, 0.1 °C
//	                   (rftypes: index 9.0, size 1.7, signed, factor 10)
//	[2]          HUMIDITY in % (HM-WDS10-TH-O; rftypes index 11.0)
//
// The HM-WDS30-T-O sends a third byte too (always 0x64 in the captures,
// e.g. “008264” = 13.0 °C); eQ-3 defines no parameter for it, so it is
// ignored.
//
// Captured frames: HM-WDS10-TH-O “006450” = 10.0 °C, 80 %; HM-WDS30-T-O
// “008264” = 13.0 °C.
package weather

import (
	"fmt"
	"html/template"
	"sync"

	"github.com/fjaeckel/hmgo/internal/bidcos"
	"github.com/fjaeckel/hmgo/internal/hm"
)

// Models.
const (
	ModelTH = "HM-WDS10-TH-O"
	ModelT  = "HM-WDS30-T-O"
)

// HasHumidity reports whether model measures humidity.
func HasHumidity(model string) bool { return model == ModelTH }

// Event is a decoded WEATHER_EVENT.
type Event struct {
	Temperature float64 // °C
	// Humidity in %; nil for the temperature-only model.
	Humidity   *int
	LowBattery bool
}

func (e *Event) String() string {
	s := fmt.Sprintf("%.1f °C", e.Temperature)
	if e.Humidity != nil {
		s += fmt.Sprintf(", %d %%", *e.Humidity)
	}
	if e.LowBattery {
		s += ", battery low"
	}
	return s
}

func (e *Event) HTML() template.HTML {
	return template.HTML(template.HTMLEscapeString("Weather: " + e.String()))
}

// DecodeEvent decodes a WEATHER_EVENT payload. humidity selects whether
// the third byte is the humidity.
func DecodeEvent(payload []byte, humidity bool) (*Event, error) {
	need := 2
	if humidity {
		need = 3
	}
	if len(payload) < need {
		return nil, fmt.Errorf("weather event: payload too short: got %d, want >= %d", len(payload), need)
	}
	t := int(payload[0]&0x7f)<<8 | int(payload[1])
	if t&0x4000 != 0 {
		t -= 0x8000
	}
	ev := &Event{
		Temperature: float64(t) / 10,
		LowBattery:  payload[0]&0x80 != 0,
	}
	if ev.Temperature < -40 || ev.Temperature > 80 {
		// Outside the sensors' range per eQ-3 (-40 … 80 °C).
		return nil, fmt.Errorf("weather event: temperature %.1f °C out of range", ev.Temperature)
	}
	if humidity {
		h := int(payload[2])
		if h > 100 {
			return nil, fmt.Errorf("weather event: humidity %d %% out of range", h)
		}
		ev.Humidity = &h
	}
	return ev, nil
}

// Sensor is an outdoor weather sensor.
type Sensor struct {
	hm.StandardDevice

	latestMu sync.RWMutex
	latest   *Event
}

// NewSensor returns a sensor of the given model. sd.BCS may be nil.
func NewSensor(sd hm.StandardDevice, model string) (*Sensor, error) {
	if model != ModelTH && model != ModelT {
		return nil, fmt.Errorf("weather: unsupported model %q", model)
	}
	sd.NumChannels = 1
	sd.ModelName = model
	return &Sensor{StandardDevice: sd}, nil
}

func (s *Sensor) HomeMaticType() string { return "weather" }

func (s *Sensor) MostRecentEvents() []hm.Event {
	s.latestMu.RLock()
	defer s.latestMu.RUnlock()
	if s.latest == nil {
		return nil
	}
	return []hm.Event{s.latest}
}

// Decode decodes WEATHER_EVENT → *Event, the common frames, and returns
// nil, nil for anything else.
func (s *Sensor) Decode(pkt *bidcos.Packet) (any, error) {
	if err := s.CheckIncoming(pkt); err != nil {
		return nil, err
	}
	if pkt.Cmd != bidcos.WeatherEvent {
		return hm.DecodeCommon(pkt)
	}
	ev, err := DecodeEvent(pkt.Payload, HasHumidity(s.ModelName))
	if err != nil {
		return nil, err
	}
	s.latestMu.Lock()
	s.latest = ev
	s.latestMu.Unlock()
	return ev, nil
}
