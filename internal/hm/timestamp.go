// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: MIT

package hm

import (
	"time"

	"github.com/fjaeckel/hmgo/internal/bidcos"
)

// Time requests (BidCoS cmd 0x3F).
//
// HM-CC-RT-DN and HM-TC-IT-WM-W-EU (and the older HM-CC-TC) send a time
// request (cmd 0x3F) to their central, e.g. after battery insertion and
// periodically, to set their clock (needed for the week program). FHEM
// answers it for exactly these three models (CUL_HM_Parse, “Timestamp
// request”, only when the request is addressed to FHEM's own ID):
//
//	$mNo 80 3F $ioId $src 0202 $s2000
//
// i.e. the response reuses the request's message counter, flags 0x80
// (RepeatEnable, no BiDi: the device does not ACK it), cmd 0x3F, and
// payload 02 02 followed by 4 bytes (big endian) of
// CUL_HM_secSince2000() = local time in seconds since 2000-01-01 minus
// 3600 (“HM Special”). The second byte is a UTC offset in units of 30
// minutes, which FHEM hard-codes to 2 (= +1 h).
//
// Homegear (HomeMaticCentral::handleTimeRequest) sends the general form:
//
//	payload 02 <int8 UTC offset in 30 min units> <uint32 BE UTC seconds since 2000-01-01>
//
// The device computes its local time as seconds + offset*30min. For a
// German setup both are identical: winter (CET, +1h) FHEM sends
// offset 2 and local-1h = UTC; summer (CEST, +2h) FHEM sends offset 2
// and local-1h = UTC+1h, which yields the same local time as Homegear's
// offset 4 and UTC. We implement the Homegear form, which is correct in
// every time zone (FHEM's is only correct for CET/CEST).
//
// Timezone handling: the offset is taken from now's location (use
// now.In(loc) to choose; time.Now() uses the system's local zone,
// i.e. /etc/localtime or $TZ). DST changes are therefore only
// picked up by the device when it requests the time again (the devices
// do that regularly, and also daily around the DST switch).

// Seconds between the Unix epoch and 2000-01-01T00:00:00Z.
const epoch2000 = 946684800

// TimestampResponse returns the answer to a time request (cmd 0x3F)
// from dst. reqMsgcnt is the message counter of the request; the
// response always echoes it, including 0.
func TimestampResponse(reqMsgcnt byte, central, dst [3]byte, now time.Time) *bidcos.Packet {
	_, off := now.Zone()
	secs := uint32(now.Unix() - epoch2000)
	return &bidcos.Packet{
		Msgcnt:      reqMsgcnt,
		FixedMsgcnt: true,
		Flags:       bidcos.RepeatEnable,
		Cmd:         bidcos.Timestamp,
		Source:      central,
		Dest:        dst,
		Payload: []byte{
			0x02,
			byte(int8(off / 1800)),
			byte(secs >> 24),
			byte(secs >> 16),
			byte(secs >> 8),
			byte(secs),
		},
	}
}
