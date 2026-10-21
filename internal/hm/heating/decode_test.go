package heating_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/stapelberg/hmgo/internal/bidcos"
	"github.com/stapelberg/hmgo/internal/hm"
	"github.com/stapelberg/hmgo/internal/hm/heating"
	"github.com/stapelberg/hmgo/internal/hm/hmtest"
)

var (
	central = [3]byte{0x1a, 0x2b, 0x3c}
	rtAddr  = [3]byte{0x38, 0xf5, 0x9c}
)

func newRT(bcs *bidcos.Sender) *heating.Thermostat {
	return heating.NewThermostat(hm.NewStandardDevice(bcs, rtAddr, "Wohnzimmer"))
}

func TestModel(t *testing.T) {
	rt := newRT(nil)
	var _ hm.Device = rt
	if rt.Model() != "HM-CC-RT-DN" || rt.Channels() != 6 || rt.AddrHex() != "38f59c" {
		t.Errorf("model %q channels %d addr %s", rt.Model(), rt.Channels(), rt.AddrHex())
	}
}

func TestDecodeInfoLevel(t *testing.T) {
	rt := newRT(nil)
	// INFO_LEVEL (flags 0x86 = RepeatEnable|Broadcast|WakeMeUp), payload:
	// 0a  subtype
	// a8  set temp 42/2 = 21.0, actual hi bits 00
	// c6  actual 198/10 = 19.8
	// c7  fault 6 (lowbat), battery 7/10+1.5 = 2.2 V
	// 23  valve 35 %
	// c5  mode 3 (boost), 5 minutes
	ev, err := rt.Decode(hmtest.Packet(0x01, 0x48, 0x3c, 0x86, 0x10, rtAddr, [3]byte{}, 0x0a, 0xa8, 0xc6, 0xc7, 0x23, 0xc5))
	if err != nil {
		t.Fatal(err)
	}
	ie, ok := ev.(*heating.InfoEvent)
	if !ok {
		t.Fatalf("got %T, want *heating.InfoEvent", ev)
	}
	want := heating.InfoEvent{
		SetTemperature:    21.0,
		ActualTemperature: 19.8,
		Fault:             heating.Lowbat,
		BatteryState:      2.2,
		ValveState:        35,
		Control:           heating.BoostMode,
		BoostState:        5,
	}
	if *ie != want {
		t.Errorf("got %+v, want %+v", *ie, want)
	}
	if !ie.LowBattery() {
		t.Error("LowBattery() = false")
	}
	if got := rt.MostRecentEvents(); len(got) != 1 {
		t.Errorf("MostRecentEvents: %d", len(got))
	}

	// party mode frame: 6 more bytes, still decodes
	ev, err = rt.Decode(hmtest.Packet(0x01, 0x48, 0x3d, 0x86, 0x10, rtAddr, [3]byte{}, 0x0a, 0xb0, 0xe2, 0x08, 0x00, 0x80, 0x10, 0x05, 0x18, 0x14, 0x06, 0x18, 0x55, 0x28))
	if err != nil {
		t.Fatal(err)
	}
	if ie := ev.(*heating.InfoEvent); ie.Control != heating.PartyMode || ie.SetTemperature != 22 {
		t.Errorf("party: %+v", ie)
	}

	if _, err := rt.DecodeInfoEvent([]byte{0x0a, 0xb0, 0xe2}); err == nil {
		t.Error("short payload: want error")
	}
}

func TestDecodeAckStatus(t *testing.T) {
	rt := newRT(nil)
	for _, tt := range []struct {
		name    string
		payload []byte
		want    heating.AckStatusEvent
	}{
		{
			// 01 04 2c 00 3a 40: channel 4, 22.0 °C, ok, RSSI -58, manual
			name:    "manual",
			payload: []byte{0x01, 0x04, 0x2c, 0x00, 0x3a, 0x40},
			want:    heating.AckStatusEvent{Channel: 4, SetTemperature: 22, HasControl: true, Control: heating.ManuMode, DeviceRSSI: -58},
		},
		{
			// lowbat bit + fault 1 (valve tight), boost 4 min, rssi unknown
			name:    "lowbat boost",
			payload: []byte{0x01, 0x04, 0x2a, 0x82, 0x00, 0xc4},
			want:    heating.AckStatusEvent{Channel: 4, SetTemperature: 21, LowBattery: true, Fault: heating.ValveTight, HasControl: true, Control: heating.BoostMode, BoostState: 4},
		},
		{
			name:    "short",
			payload: []byte{0x01, 0x04, 0x09, 0x00},
			want:    heating.AckStatusEvent{Channel: 4, SetTemperature: 4.5},
		},
	} {
		ev, err := rt.Decode(hmtest.Packet(0x01, 0x40, 0x12, 0x80, 0x02, rtAddr, central, tt.payload...))
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		as, ok := ev.(*heating.AckStatusEvent)
		if !ok {
			t.Fatalf("%s: got %T", tt.name, ev)
		}
		if *as != tt.want {
			t.Errorf("%s: got %+v, want %+v", tt.name, *as, tt.want)
		}
	}
}

func TestDecodeOther(t *testing.T) {
	rt := newRT(nil)
	// team frame (0x59): 0x59 = 22.0 °C <<2 | manual
	ev, err := rt.Decode(hmtest.Packet(0x01, 0x40, 0x12, 0xa2, 0x59, rtAddr, [3]byte{0x38, 0xf5, 0xa1}, 0xb1))
	if err != nil {
		t.Fatal(err)
	}
	if te, ok := ev.(*heating.TeamEvent); !ok || te.SetTemperature != 22 || te.Control != heating.ManuMode {
		t.Errorf("team: %#v", ev)
	}
	// time request
	ev, err = rt.Decode(hmtest.Packet(0x01, 0x40, 0x21, 0xa0, 0x3f, rtAddr, central, 0x04, 0x00))
	if err != nil {
		t.Fatal(err)
	}
	if tr, ok := ev.(*hm.TimeRequest); !ok || tr.Msgcnt != 0x21 {
		t.Errorf("time request: %#v", ev)
	}
	// plain ACK: nothing
	ev, err = rt.Decode(hmtest.Packet(0x01, 0x40, 0x21, 0x80, 0x02, rtAddr, central, 0x00))
	if ev != nil || err != nil {
		t.Errorf("plain ack: %v %v", ev, err)
	}
	// other device
	if _, err := rt.Decode(hmtest.Packet(0x01, 0x40, 0x21, 0x80, 0x02, [3]byte{1, 2, 3}, central, 0x00)); err == nil {
		t.Error("other device: want error")
	}
	// AES failed
	if _, err := rt.Decode(hmtest.Packet(0x03, 0x40, 0x21, 0x86, 0x10, rtAddr, central, 0x0a, 0xb0, 0xe2, 0x08, 0x00, 0x00)); err == nil {
		t.Error("aes failed: want error")
	}
}

func TestPackets(t *testing.T) {
	p, err := heating.SetTemperaturePacket(rtAddr, 21.5)
	if err != nil {
		t.Fatal(err)
	}
	check := func(name string, p *bidcos.Packet, want []byte) {
		t.Helper()
		if p.Cmd != 0x11 || p.Flags != 0xb0 || p.Dest != rtAddr || !bytes.Equal(p.Payload, want) {
			t.Errorf("%s: cmd %02x flags %02x dest %x payload % x, want % x", name, p.Cmd, p.Flags, p.Dest, p.Payload, want)
		}
	}
	check("set", p, []byte{0x86, 0x04, 0x2b})
	check("auto", heating.AutoModePacket(rtAddr), []byte{0x80, 0x04})
	p, err = heating.ManualModePacket(rtAddr, 30.5)
	if err != nil {
		t.Fatal(err)
	}
	check("manual", p, []byte{0x81, 0x04, 0x3d})
	check("boost", heating.BoostPacket(rtAddr), []byte{0x83, 0x04})
	if _, err := heating.SetTemperaturePacket(rtAddr, 31); err == nil {
		t.Error("31 °C: want error")
	}
}

func TestCommands(t *testing.T) {
	tr := &hmtest.Transport{Respond: hmtest.AckWith(0x01, 0x04, 0x2b, 0x00, 0x3a, 0x40)}
	rt := newRT(bidcos.NewSender(tr, central))
	ctx := context.Background()
	st, err := rt.SetManualMode(ctx, 21.5)
	if err != nil {
		t.Fatal(err)
	}
	if st == nil || st.SetTemperature != 21.5 || st.Control != heating.ManuMode {
		t.Errorf("status: %+v", st)
	}
	if got := tr.Sent[0].Payload; !bytes.Equal(got, []byte{0x81, 0x04, 0x2b}) {
		t.Errorf("sent % x", got)
	}

	tr.Respond = hmtest.AckWith(0x00)
	for name, f := range map[string]func() (*heating.AckStatusEvent, error){
		"set":   func() (*heating.AckStatusEvent, error) { return rt.SetTemperature(ctx, 20) },
		"auto":  func() (*heating.AckStatusEvent, error) { return rt.SetAutoMode(ctx) },
		"boost": func() (*heating.AckStatusEvent, error) { return rt.SetBoost(ctx) },
	} {
		st, err := f()
		if err != nil || st != nil {
			t.Errorf("%s: plain ack: %v %v", name, st, err)
		}
	}

	tr.Respond = hmtest.AckWith(0x80)
	if _, err := rt.SetAutoMode(ctx); err == nil {
		t.Error("NACK: want error")
	}
}
