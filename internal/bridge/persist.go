package bridge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/fjaeckel/hmgo/internal/ha"
)

// persistedDevice is the on-disk form of a device's state.
type persistedDevice struct {
	State          ha.State `json:"state"`
	LastOnTarget   float64  `json:"last_on_target,omitempty"`
	PreBoostHAMode string   `json:"pre_boost_ha_mode,omitempty"`
}

type persistedState struct {
	Version int                        `json:"version"`
	Saved   time.Time                  `json:"saved"`
	Devices map[string]persistedDevice `json:"devices"` // by lowercase address
}

// loadState restores the last known device state from path. A missing
// file is not an error. Message counters are not restored: they count
// messages since the bridge started.
func (b *Bridge) loadState(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var ps persistedState
	if err := json.Unmarshal(data, &ps); err != nil {
		return fmt.Errorf("%s: %v (ignoring it)", path, err)
	}
	n := 0
	for _, d := range b.devices {
		pd, ok := ps.Devices[d.desc.Addr]
		if !ok {
			continue
		}
		pd.State.MessagesReceived = nil
		pd.State.LastCommand = ""
		d.state = pd.State
		d.lastOnTarget = pd.LastOnTarget
		d.preBoostHAMode = pd.PreBoostHAMode
		deriveState(d)
		n++
	}
	for _, g := range b.groups {
		g.state = groupState(g)
	}
	b.logf("restored the last known state of %d devices from %s (saved %v)", n, path, ps.Saved.Format(time.RFC3339))
	return nil
}

// saveState writes the current device state to path atomically. Caller
// must not hold b.mu.
func (b *Bridge) saveState(path string) error {
	b.mu.Lock()
	ps := persistedState{Version: 1, Saved: b.now(), Devices: make(map[string]persistedDevice)}
	for _, d := range b.devices {
		if d.state.LastSeen.IsZero() {
			continue
		}
		var st ha.State
		st.Merge(d.state)
		ps.Devices[d.desc.Addr] = persistedDevice{State: st, LastOnTarget: d.lastOnTarget, PreBoostHAMode: d.preBoostHAMode}
	}
	b.stateDirty = false
	b.mu.Unlock()

	data, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// checkAvailability marks devices that have been silent for longer than
// their stale timeout as unavailable (and back). Devices never heard
// since startup are measured from the start time, so a restart does not
// make everything unavailable at once. Caller holds b.mu.
func (b *Bridge) checkAvailability(now time.Time) []ha.Message {
	var msgs []ha.Message
	var topics ha.TopicConfig
	if b.pub != nil {
		topics = b.pub.Topics()
	}
	for _, d := range b.devices {
		ref := d.state.LastSeen
		if ref.Before(b.started) && !d.heardSinceStart {
			// Restored state: give the device its full timeout after
			// the start before calling it stale.
			ref = b.started
		}
		online := now.Sub(ref) < d.cfg.StaleTimeout()
		if online != d.available || !d.availPublished {
			if online != d.available {
				if online {
					b.logf("%s (%s) is reporting again", d.cfg.Name, d.desc.Addr)
				} else {
					b.logf("%s (%s) has been silent since %v; marking it unavailable (battery empty or out of range?)", d.cfg.Name, d.desc.Addr, lastSeenString(d.state.LastSeen))
				}
			}
			d.available = online
			if b.pub != nil {
				d.availPublished = true
				msgs = append(msgs, ha.DeviceAvailabilityMessage(topics, d.desc.Addr, online))
			}
		}
		f := 0.0
		if online {
			f = 1
		}
		metricAvailable.WithLabelValues(d.desc.Addr, d.cfg.Name, d.cfg.Model).Set(f)
	}
	return msgs
}

func lastSeenString(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format(time.RFC3339)
}

// systemClockSynced reports whether the kernel considers the system
// clock synchronised (systemd-timesyncd, chrony and ntpd clear
// STA_UNSYNC once they have synchronised).
func systemClockSynced() bool {
	var tx unix.Timex
	if _, err := unix.Adjtimex(&tx); err != nil {
		return true // can't tell; don't block time handling
	}
	return tx.Status&unix.STA_UNSYNC == 0
}
