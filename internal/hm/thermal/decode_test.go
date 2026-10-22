// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: MIT

package thermal_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/fjaeckel/hmgo/internal/bidcos"
	"github.com/fjaeckel/hmgo/internal/hm"
	"github.com/fjaeckel/hmgo/internal/hm/hmtest"
	"github.com/fjaeckel/hmgo/internal/hm/thermal"
)

var (
	central = [3]byte{0x1a, 0x2b, 0x3c}
	tcAddr  = [3]byte{0x2a, 0x1b, 0x3c}
)

func newTC(bcs *bidcos.Sender) *thermal.ThermalControl {
	return thermal.NewThermalControl(hm.NewStandardDevice(bcs, tcAddr, "Wand"))
}

func TestModel(t *testing.T) {
	tc := newTC(nil)
	var _ hm.Device = tc
	if tc.Model() != "HM-TC-IT-WM-W-EU" || tc.Channels() != 7 {
		t.Errorf("model %q channels %d", tc.Model(), tc.Channels())
	}
}

func TestDecodeInfoLevel(t *testing.T) {
	tc := newTC(nil)
	// 0b  subtype
	// 54  set temp 0x54>>2 = 21 → 10.5 °C, actual hi bits 00
	// c8  actual 20.0
	// eb  lowbat, comm, window open, battery 0x0b/10+1.5 = 2.6 V
	// 40  manual
	ev, err := tc.Decode(hmtest.Packet(0x01, 0x50, 0x0a, 0xa2, 0x10, tcAddr, [3]byte{}, 0x0b, 0x54, 0xc8, 0xeb, 0x40))
	if err != nil {
		t.Fatal(err)
	}
	ie, ok := ev.(*thermal.InfoEvent)
	if !ok {
		t.Fatalf("got %T", ev)
	}
	want := thermal.InfoEvent{
		SetTemperature:         10.5,
		ActualTemperature:      20.0,
		LowbatReporting:        true,
		CommunicationReporting: true,
		WindowOpenReporting:    true,
		BatteryState:           2.6,
		Control:                thermal.ManuMode,
	}
	if *ie != want {
		t.Errorf("got %+v, want %+v", *ie, want)
	}

	// Only the window bit set: must not be confused with the other bits
	// (the old decoder compared payload[3]>>5 == 1, which fails as soon
	// as lowbat is set too).
	ie, err = tc.DecodeInfoEvent([]byte{0x0b, 0xb0, 0xdf, 0xae, 0xc3})
	if err != nil {
		t.Fatal(err)
	}
	if !ie.LowbatReporting || ie.CommunicationReporting || !ie.WindowOpenReporting || ie.BatteryState != 2.9 || ie.Control != thermal.BoostMode || ie.BoostState != 3 {
		t.Errorf("got %+v", ie)
	}

	// negative temperature: 10 bit signed, -2.0 °C = 0x3ec
	ie, err = tc.DecodeInfoEvent([]byte{0x0b, 0x13, 0xec, 0x0e, 0x00})
	if err != nil {
		t.Fatal(err)
	}
	if ie.ActualTemperature != -2.0 || ie.SetTemperature != 2.0 {
		t.Errorf("negative: %+v", ie)
	}

	if _, err := tc.DecodeInfoEvent([]byte{0x0a, 0xb0, 0xdf, 0x0e, 0x00}); err == nil {
		t.Error("wrong subtype: want error")
	}
}

func TestDecodeAckStatus(t *testing.T) {
	tc := newTC(nil)
	// 01 02 2c 86 45 00: channel 2, 22.0 °C, lowbat+comm+window, RSSI -69, auto
	ev, err := tc.Decode(hmtest.Packet(0x01, 0x40, 0x12, 0x80, 0x02, tcAddr, central, 0x01, 0x02, 0x2c, 0x86, 0x45, 0x00))
	if err != nil {
		t.Fatal(err)
	}
	as, ok := ev.(*thermal.AckStatusEvent)
	if !ok {
		t.Fatalf("got %T", ev)
	}
	want := thermal.AckStatusEvent{Channel: 2, SetTemperature: 22, LowbatReporting: true, CommunicationReporting: true, WindowOpenReporting: true, HasControl: true, Control: thermal.AutoMode, DeviceRSSI: -69}
	if *as != want {
		t.Errorf("got %+v, want %+v", *as, want)
	}
}

func TestDecodeEvents(t *testing.T) {
	tc := newTC(nil)
	// THERMALCONTROL_EVENT 0x5a to the RT-DN team: b0 e6 37 = 22.0 °C set, 23.0 actual, 55 %
	ev, err := tc.Decode(hmtest.Packet(0x01, 0x40, 0x30, 0xa2, 0x5a, tcAddr, [3]byte{0x38, 0xf5, 0x9c}, 0xb0, 0xe6, 0x37))
	if err != nil {
		t.Fatal(err)
	}
	if tce, ok := ev.(*thermal.ThermalControlEvent); !ok || tce.SetTemperature != 22 || tce.ActualTemperature != 23 || tce.ActualHumidity != 55 {
		t.Errorf("thermal control: %#v", ev)
	}
	// WEATHER_EVENT 0x70: 00 e6 37 = 23.0 °C, 55 %
	ev, err = tc.Decode(hmtest.Packet(0x01, 0x40, 0x31, 0x86, 0x70, tcAddr, [3]byte{}, 0x00, 0xe6, 0x37))
	if err != nil {
		t.Fatal(err)
	}
	if we, ok := ev.(*thermal.WeatherEvent); !ok || we.Temperature != 23 || we.Humidity != 55 {
		t.Errorf("weather: %#v", ev)
	}
	// negative weather temperature: -5.5 °C = 0x7fc9 (15 bit)
	we, err := tc.DecodeWeatherEvent([]byte{0x7f, 0xc9, 0x50})
	if err != nil {
		t.Fatal(err)
	}
	if we.Temperature != -5.5 {
		t.Errorf("negative weather: %v", we.Temperature)
	}
	// SWITCH_EVENT 0x41 on channel 7
	ev, err = tc.Decode(hmtest.Packet(0x01, 0x40, 0x32, 0xa2, 0x41, tcAddr, [3]byte{1, 2, 3}, 0x87, 0x05, 0xc8))
	if err != nil {
		t.Fatal(err)
	}
	if se, ok := ev.(*thermal.SwitchEvent); !ok || se.Channel != 7 || !se.LowBattery || se.Counter != 5 || se.DecisionValue != 200 {
		t.Errorf("switch: %#v", ev)
	}
	// time request
	ev, err = tc.Decode(hmtest.Packet(0x01, 0x40, 0x33, 0xa0, 0x3f, tcAddr, central, 0x04, 0x00))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ev.(*hm.TimeRequest); !ok {
		t.Errorf("time request: %#v", ev)
	}
	if got := len(tc.MostRecentEvents()); got != 2 {
		t.Errorf("MostRecentEvents: %d", got)
	}
}

func TestCommands(t *testing.T) {
	tr := &hmtest.Transport{Respond: hmtest.AckWith(0x01, 0x02, 0x2a, 0x00, 0x3a, 0xc5)}
	tc := newTC(bidcos.NewSender(tr, central))
	ctx := context.Background()
	st, err := tc.SetBoost(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st == nil || st.Control != thermal.BoostMode || st.BoostState != 5 || st.SetTemperature != 21 {
		t.Errorf("status: %+v", st)
	}
	if _, err := tc.SetTemperature(ctx, 19); err != nil {
		t.Fatal(err)
	}
	if _, err := tc.SetAutoMode(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := tc.SetManualMode(ctx, 4.5); err != nil {
		t.Fatal(err)
	}
	wants := [][]byte{{0x83, 0x02}, {0x86, 0x02, 0x26}, {0x80, 0x02}, {0x81, 0x02, 0x09}}
	for i, w := range wants {
		p := tr.Sent[i]
		if p.Cmd != 0x11 || p.Flags != 0xb0 || p.Dest != tcAddr || p.Source != central || !bytes.Equal(p.Payload, w) {
			t.Errorf("sent[%d] = %+v, want payload % x", i, p, w)
		}
	}
	if _, err := tc.SetManualMode(ctx, 3); err == nil {
		t.Error("3 °C: want error")
	}
	p, err := thermal.SetTemperaturePacket(tcAddr, 20)
	if err != nil || !bytes.Equal(p.Payload, []byte{0x86, 0x02, 0x28}) {
		t.Errorf("SetTemperaturePacket: % x %v", p.Payload, err)
	}
	if p, _ := thermal.ManualModePacket(tcAddr, 20); !bytes.Equal(p.Payload, []byte{0x81, 0x02, 0x28}) {
		t.Errorf("ManualModePacket: % x", p.Payload)
	}
	if p := thermal.AutoModePacket(tcAddr); !bytes.Equal(p.Payload, []byte{0x80, 0x02}) {
		t.Errorf("AutoModePacket: % x", p.Payload)
	}
	if p := thermal.BoostPacket(tcAddr); !bytes.Equal(p.Payload, []byte{0x83, 0x02}) {
		t.Errorf("BoostPacket: % x", p.Payload)
	}
}
