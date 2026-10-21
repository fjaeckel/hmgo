// Package hmtest provides helpers for testing device models without a
// radio module.
package hmtest

import (
	"context"
	"sync"

	"github.com/fjaeckel/hmgo/internal/bidcos"
)

// Frame builds a received frame in the format accepted by bidcos.Decode.
// status uses the receive numbering (0x01 normal, 0x02 AES OK, 0x03
// AES failed).
func Frame(status, rssi, cnt, flags, cmd byte, src, dst [3]byte, payload ...byte) []byte {
	b := []byte{status, 0x00, rssi, cnt, flags, cmd}
	b = append(b, src[:]...)
	b = append(b, dst[:]...)
	return append(b, payload...)
}

// Packet is like Frame, but returns the decoded packet.
func Packet(status, rssi, cnt, flags, cmd byte, src, dst [3]byte, payload ...byte) *bidcos.Packet {
	p, err := bidcos.Decode(Frame(status, rssi, cnt, flags, cmd, src, dst, payload...))
	if err != nil {
		panic(err)
	}
	return p
}

// Transport is a fake bidcos.Transport. It records sent packets and
// answers with Respond.
type Transport struct {
	// Respond returns the response frame (Frame format) and error for a
	// sent packet. nil Respond answers with no response.
	Respond func(sent *bidcos.Packet) ([]byte, error)

	mu   sync.Mutex
	Sent []*bidcos.Packet
}

// Send implements bidcos.Transport.
func (t *Transport) Send(ctx context.Context, encoded []byte) ([]byte, error) {
	// Packet.Encode produces the same layout as Decode accepts (status,
	// info, burst, cnt, flags, cmd, src, dst, payload).
	p, err := bidcos.Decode(encoded)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.Sent = append(t.Sent, p)
	t.mu.Unlock()
	if t.Respond == nil {
		return nil, nil
	}
	return t.Respond(p)
}

// AckWith returns a Respond func answering every packet with an ACK
// frame (cmd 0x02, flags 0x80) carrying payload, sent from the
// packet's destination.
func AckWith(payload ...byte) func(*bidcos.Packet) ([]byte, error) {
	return func(sent *bidcos.Packet) ([]byte, error) {
		return Frame(0x01, 0x40, sent.Msgcnt, 0x80, bidcos.Ack, sent.Dest, sent.Source, payload...), nil
	}
}
