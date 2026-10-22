// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bridge

import (
	"context"
	"fmt"

	"github.com/fjaeckel/hmgo/internal/hmkey"
	"github.com/fjaeckel/hmgo/internal/uartgw"
)

// pairKeyExchange gives a device that is being paired the system key
// cur. A factory-new device uses the eQ-3 default key, so that is tried
// first. A device that was unpaired (by hmbridge or a CCU) without a
// factory reset still holds the key it was given before: unpairing only
// clears its central address. It refuses the exchange from the default
// key (it challenges part 1 with its own key index, which the radio
// module does not answer for a peer registered with index 0, or it
// cannot decrypt the payload). So if part 1 fails, the exchange is
// retried once with each configured key (current, then previous) as the
// device's old key; the peer is registered with that key's index first,
// so that the module answers the device's challenge with it.
//
// peer is the device's registration (KeyIndex as before the exchange);
// it is restored if every attempt fails. On failure, part is the part
// (1 or 2) that failed.
func (b *Bridge) pairKeyExchange(ctx context.Context, peer uartgw.Peer, cur hmkey.Key) (part int, err error) {
	part, err = b.keyExchange(ctx, peer.Addr, hmkey.EQ3Default, cur)
	if err == nil || part != 1 || ctx.Err() != nil {
		return part, err
	}
	first := err
	current, previous := b.cfg.Keys()
	olds := []hmkey.Key{*current}
	if previous != nil {
		olds = append(olds, *previous)
	}
	for _, old := range olds {
		if old.Index == 0 {
			continue // that is the default key, which just failed
		}
		retry := peer
		retry.KeyIndex = old.Index
		if err := b.radio.AddPeer(ctx, retry); err != nil {
			break
		}
		b.logf("pairing: the key exchange from the eQ-3 default key failed (%v); %x may still hold key index %d from an earlier pairing, retrying with that key as its old key", first, peer.Addr[:], old.Index)
		p, err := b.keyExchange(ctx, peer.Addr, old, cur)
		if err == nil {
			b.logf("pairing: %x accepted the key exchange from key index %d", peer.Addr[:], old.Index)
			return 0, nil
		}
		if p == 2 {
			if old.Index == cur.Index && old.Bytes == cur.Bytes {
				// Part 1 was authenticated with the system key, so the
				// device holds it whether part 2 arrived or not.
				b.logf("pairing: %x holds the system key (part 2 of the exchange: %v)", peer.Addr[:], err)
				return 0, nil
			}
			// The device knows this older key; whether it switched to
			// the new one is unknown.
			b.radio.AddPeer(ctx, peer)
			return 2, fmt.Errorf("from key index %d: %w", old.Index, err)
		}
		if ctx.Err() != nil {
			break
		}
	}
	if err := b.radio.AddPeer(ctx, peer); err != nil {
		b.logf("pairing: restoring the radio module registration of %x: %v", peer.Addr[:], err)
	}
	return 1, first
}
