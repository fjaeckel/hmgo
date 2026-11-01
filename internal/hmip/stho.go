// SPDX-License-Identifier: BSD-3-Clause

package hmip

import (
	"encoding/binary"
	"fmt"
)

// STHOStatus is the cyclic status report of an HmIP-STHO
// (temperature/humidity sensor, outdoor).
type STHOStatus struct {
	Temperature float64 // °C
	Humidity    int     // % relative humidity
	Voltage     float64 // battery voltage, V
}

// sthoStatusLen is the length of the STHO's STATUS payload after the
// application header.
const sthoStatusLen = 13

// ParseSTHOStatus decodes the payload (after the application header) of
// an HmIP-STHO's STATUS frame. Layout from captures, checked against the
// values OpenCCU showed:
//
//	byte 4:      operating voltage, 0.1 V
//	bytes 10-11: temperature, 0.1 °C, big endian
//	byte 12:     relative humidity, %
//
// The other bytes are unknown, and so is whether other STATUS frames of
// the device look different; frames of another length are rejected.
// Temperatures below 0 °C have not been recorded yet; two's complement
// is assumed.
func ParseSTHOStatus(p []byte) (STHOStatus, error) {
	if len(p) != sthoStatusLen {
		return STHOStatus{}, fmt.Errorf("%w: STHO status of %d bytes, want %d", ErrUnsupported, len(p), sthoStatusLen)
	}
	return STHOStatus{
		Temperature: float64(int16(binary.BigEndian.Uint16(p[10:12]))) / 10,
		Humidity:    int(p[12]),
		Voltage:     float64(p[4]) / 10,
	}, nil
}
