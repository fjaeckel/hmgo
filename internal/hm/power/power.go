package power

import (
	"context"
	"sync"

	"github.com/stapelberg/hmgo/internal/bidcos"
	"github.com/stapelberg/hmgo/internal/hm"
)

// Model is the eQ-3 model name.
const Model = "HM-ES-PMSw1-Pl"

const (
	PowerMeter    = 0x02
	ChannelSwitch = 0x01
	ChannelMaster = 0x05

	On  = 0xc8
	Off = 0x00
)

const (
	MaintenanceChannel = iota
	SwitchChannel
	ConditionPowermeterChannel
	ConditionPowerChannel
	ConditionCurrentChannel
	ConditionVoltageChannel
	ConditionFrequencyChannel
)

type PowerSwitch struct {
	hm.StandardDevice

	latestPowerEvent *PowerEvent
	latestMu         sync.RWMutex
}

func (ps *PowerSwitch) HomeMaticType() string { return "power" }

func NewPowerSwitch(sd hm.StandardDevice) *PowerSwitch {
	sd.NumChannels = 6
	sd.ModelName = Model
	return &PowerSwitch{StandardDevice: sd}
}

func (ps *PowerSwitch) MostRecentEvents() []hm.Event {
	var result []hm.Event
	ps.latestMu.RLock()
	defer ps.latestMu.RUnlock()

	if ps.latestPowerEvent != nil {
		result = append(result, ps.latestPowerEvent)
	}

	return result
}

// Decode decodes POWER_EVENT (0x5e/0x5f) frames.
func (ps *PowerSwitch) Decode(pkt *bidcos.Packet) (any, error) {
	if err := ps.CheckIncoming(pkt); err != nil {
		return nil, err
	}
	switch pkt.Cmd {
	case bidcos.PowerEvent, bidcos.PowerEventCyclic:
		return ps.DecodePowerEvent(pkt.Payload)
	}
	return hm.DecodeCommon(pkt)
}

func (ps *PowerSwitch) LevelSet(ctx context.Context, channel, state, onTime byte) error {
	_, err := ps.Send(ctx, &bidcos.Packet{
		Flags: bidcos.DefaultFlags,
		Cmd:   0x11, // LevelSet
		Payload: []byte{
			0x02, // subtype
			channel,
			state,
			0x00, // constant
			onTime,
		},
	})
	return err
}
