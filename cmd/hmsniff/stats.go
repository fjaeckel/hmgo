// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"io"
	"sort"
	"time"
)

// Stats summarises a capture.
type Stats struct {
	Start      time.Time
	Frames     map[string]int // "RX LLMAC 05" → count
	BadCRC     int
	devices    map[[3]byte]*devStat
	TXToDevice int
}

type devStat struct {
	rx, txTo int
	lastRSSI int
	lastSeen time.Time
	cmds     map[byte]int
}

func newStats() *Stats {
	return &Stats{Start: time.Now(), Frames: map[string]int{}, devices: map[[3]byte]*devStat{}}
}

func (s *Stats) frame(dir string, f Frame) {
	key := fmt.Sprintf("%s %-6s", dir, dstName(f.Dst))
	if len(f.Payload) > 0 {
		key += fmt.Sprintf(" cmd %02X", f.Payload[0])
	}
	s.Frames[key]++
	if !f.CRCOK {
		s.BadCRC++
	}
}

func (s *Stats) dev(a [3]byte) *devStat {
	ds := s.devices[a]
	if ds == nil {
		ds = &devStat{cmds: map[byte]int{}}
		s.devices[a] = ds
	}
	return ds
}

func (s *Stats) message(dir string, m Msg, rssi int, d *Decoder) {
	if dir == "RX" {
		ds := s.dev(m.Src)
		ds.rx++
		ds.cmds[m.Cmd]++
		if rssi != 0 {
			ds.lastRSSI = -rssi
		}
		ds.lastSeen = time.Now()
	} else {
		s.dev(m.Dst).txTo++
	}
}

// Write prints the summary.
func (s *Stats) Write(w io.Writer, d *Decoder) {
	fmt.Fprintf(w, "\n==== summary (%v) ====\n", time.Since(s.Start).Round(time.Second))
	keys := make([]string, 0, len(s.Frames))
	for k := range s.Frames {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintln(w, "frames:")
	for _, k := range keys {
		fmt.Fprintf(w, "  %-28s %6d\n", k, s.Frames[k])
	}
	if s.BadCRC > 0 {
		fmt.Fprintf(w, "  frames with bad CRC: %d\n", s.BadCRC)
	}
	fmt.Fprintln(w, "devices (rx = messages from, tx = messages to):")
	addrs := make([][3]byte, 0, len(s.devices))
	for a := range s.devices {
		addrs = append(addrs, a)
	}
	sort.Slice(addrs, func(i, j int) bool { return fmt.Sprintf("%x", addrs[i]) < fmt.Sprintf("%x", addrs[j]) })
	for _, a := range addrs {
		ds := s.devices[a]
		cmds := ""
		for c, n := range ds.cmds {
			name := cmdNames[c]
			if name == "" {
				name = fmt.Sprintf("%02X", c)
			}
			cmds += fmt.Sprintf(" %s×%d", name, n)
		}
		fmt.Fprintf(w, "  %-45s rx %4d  tx %4d  rssi %4d %s\n", d.name(a), ds.rx, ds.txTo, ds.lastRSSI, cmds)
	}
	var silent []string
	for a, di := range d.Devices {
		if ds := s.devices[a]; ds == nil || ds.rx == 0 {
			silent = append(silent, fmt.Sprintf("%02X%02X%02X(%s)", a[0], a[1], a[2], di))
		}
	}
	sort.Strings(silent)
	if len(silent) > 0 {
		fmt.Fprintf(w, "configured devices not heard yet (%d):\n", len(silent))
		for _, s := range silent {
			fmt.Fprintf(w, "  %s\n", s)
		}
	}
}
