// SPDX-License-Identifier: BSD-3-Clause

package bridge

import (
	"time"

	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/ha"
	"github.com/fjaeckel/hmgo/internal/hmip"
)

// Homematic IP devices of the configuration (hmip.devices): their
// STATUS frames, passed on by the HMIPMonitor, are decoded and published
// like a weather sensor's. Receive only; they are not BidCoS devices, so
// they are kept apart from b.devices (no peers, AES, polls or settings).

// hmipQueue bounds the reports waiting for the event loop.
const hmipQueue = 16

type hmipSensor struct {
	cfg   *config.HMIPDevice
	desc  ha.DeviceDesc
	state ha.State
	msgs  uint64
	// See device.available and device.availPublished.
	available, availPublished bool
}

// hmipReport is a STATUS frame from a Homematic IP device.
type hmipReport struct {
	src, dst hmip.Address
	rssi     *int
	payload  []byte // after the application header; owned by the report
}

// setupHMIP creates the configured Homematic IP sensors and has the
// monitor report their frames to the event loop.
func (b *Bridge) setupHMIP() {
	h := b.cfg.HMIP
	if h == nil {
		return
	}
	if b.hmip == nil {
		b.logf("hmip: no Homematic IP monitor; hmip.devices are not received")
		return
	}
	b.hmipBy = make(map[hmip.Address]*hmipSensor)
	for i := range h.Devices {
		dc := &h.Devices[i]
		dc.Name = labelValue(dc.Name)
		s := &hmipSensor{
			cfg:       dc,
			desc:      ha.DeviceDesc{Addr: dc.AddrHex(), Name: dc.Name, Model: dc.Model, Kind: ha.WeatherSensor},
			available: true,
		}
		b.hmipSensors = append(b.hmipSensors, s)
		b.hmipBy[hmip.Address(dc.Addr())] = s
	}
	b.hmipC = make(chan hmipReport, hmipQueue)
	configured := make(map[hmip.Address]bool, len(b.hmipBy))
	for a := range b.hmipBy {
		configured[a] = true
	}
	b.hmip.setReporter(configured, func(r hmipReport) {
		select {
		case b.hmipC <- r:
		default:
			b.debugf("hmip: event loop busy, dropping a frame from %v", r.src)
		}
	})
}

// handleHMIPReport applies a STATUS frame. Runs on the event loop.
func (b *Bridge) handleHMIPReport(r hmipReport) {
	s := b.hmipBy[r.src]
	if s == nil {
		return // not configured; listed by the monitor
	}
	if want := hmip.Address(b.cfg.HMIP.Addr()); r.dst != want {
		b.debugf("hmip: %s (%s): ignoring a frame to %v (hmip.address is %v)", s.cfg.Name, s.desc.Addr, r.dst, want)
		return
	}
	st, err := hmip.ParseSTHOStatus(r.payload)
	if err != nil {
		b.floodLog.Logf(b.logf, floodKeyDecode, "%s (%s): status %x: %v", s.cfg.Name, s.desc.Addr, r.payload, err)
		return
	}
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	s.msgs++
	s.state.Merge(ha.State{
		CurrentTemperature: ptr(st.Temperature),
		Humidity:           ptr(st.Humidity),
		BatteryVoltage:     ptr(st.Voltage),
		RSSI:               r.rssi,
		LastSeen:           now,
		MessagesReceived:   ptr(s.msgs),
	})
	setDeviceMetrics(s.desc.Addr, s.cfg.Name, s.cfg.Model, &s.state, s.msgs)
	var msgs []ha.Message
	if !s.available {
		msgs = b.checkAvailability(now)
	}
	if b.pub != nil {
		if m, err := ha.StateMessage(b.pub.Topics(), s.desc.Addr, s.state); err == nil {
			msgs = append(msgs, m)
		}
	}
	b.outbox.put(msgs...)
}

// hmipDiscoveryLocked returns discovery, state and availability of the
// Homematic IP sensors for PublishAll. b.mu must be held.
func (b *Bridge) hmipDiscoveryLocked(topics ha.TopicConfig, stale func(ha.TopicConfig, ha.DeviceDesc) []ha.Message) []ha.Message {
	var msgs []ha.Message
	for _, s := range b.hmipSensors {
		msgs = append(msgs, ha.DiscoveryMessages(topics, s.desc)...)
		msgs = append(msgs, stale(topics, s.desc)...)
		if !s.state.LastSeen.IsZero() {
			if m, err := ha.StateMessage(topics, s.desc.Addr, s.state); err == nil {
				msgs = append(msgs, m)
			}
		}
		msgs = append(msgs, ha.DeviceAvailabilityMessage(topics, s.desc.Addr, s.available))
		s.availPublished = true
	}
	return msgs
}

// hmipAvailabilityLocked is checkAvailability for the Homematic IP
// sensors. b.mu must be held.
func (b *Bridge) hmipAvailabilityLocked(now time.Time, topics ha.TopicConfig) []ha.Message {
	var msgs []ha.Message
	for _, s := range b.hmipSensors {
		ref := s.state.LastSeen
		if ref.IsZero() {
			ref = b.started
		}
		stale := s.cfg.StaleTimeout()
		online := stale == 0 || now.Sub(ref) < stale
		if online != s.available {
			if online {
				b.logf("%s (%s) is reporting again", s.cfg.Name, s.desc.Addr)
			} else {
				b.logf("%s (%s) has been silent since %v; marking it unavailable (battery empty or out of range?)", s.cfg.Name, s.desc.Addr, lastSeenString(s.state.LastSeen))
			}
		}
		if online != s.available || !s.availPublished {
			s.available = online
			if b.pub != nil {
				s.availPublished = true
				msgs = append(msgs, ha.DeviceAvailabilityMessage(topics, s.desc.Addr, online))
			}
		}
		metricAvailable.WithLabelValues(s.desc.Addr, s.cfg.Name, s.cfg.Model).Set(boolFloat(online))
	}
	return msgs
}

// logHMIP logs the Homematic IP sensors at startup.
func (b *Bridge) logHMIP() {
	for _, s := range b.hmipSensors {
		b.logf("hmip %s %s (%s): receive only, reports to %v", s.desc.Addr, s.cfg.Name, s.cfg.Model, hmip.Address(b.cfg.HMIP.Addr()))
	}
}

// hmipByAddrHex returns the sensor with the address addr (lowercase hex).
func (b *Bridge) hmipByAddrHex(addr string) *hmipSensor {
	for _, s := range b.hmipSensors {
		if s.desc.Addr == addr {
			return s
		}
	}
	return nil
}

// apiHMIPLocked is apiDeviceLocked for a Homematic IP sensor. b.mu must
// be held.
func apiHMIPLocked(s *hmipSensor) APIDevice {
	var st ha.State
	st.Merge(s.state)
	res := APIDevice{
		Name:      s.cfg.Name,
		Model:     s.cfg.Model,
		Address:   s.desc.Addr,
		Source:    "hmip",
		Available: s.available,
		RSSI:      st.RSSI,
		Messages:  s.msgs,
		Summary:   summary(s.desc.Kind, &st, ""),
		State:     st,
		AES:       APIAES{Channels: []int{}},
	}
	if !st.LastSeen.IsZero() {
		t := st.LastSeen
		res.LastSeen = &t
		res.LastEvent = "status"
	}
	return res
}
