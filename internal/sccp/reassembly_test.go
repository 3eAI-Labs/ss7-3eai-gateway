// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package sccp

import (
	"bytes"
	"testing"
	"time"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/m3ua"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// segments splits data into n XUDT segments as a peer would (Q.714
// §4.1.1.1.2): same addresses, class 1, F in the first, remaining counting
// down, one local reference.
func segments(t *testing.T, data []byte, n int, ref uint32, returnOnError bool) [][]byte {
	t.Helper()
	size := (len(data) + n - 1) / n
	var out [][]byte
	for i := 0; i < n; i++ {
		end := min((i+1)*size, len(data))
		msg := &SccpMessage{
			MessageType:    MsgTypeXUDT,
			ProtocolClass:  ClassInSequenceConnectionless,
			ReturnOnError:  returnOnError && i == 0,
			HopCounter:     15,
			CalledAddress:  gtAddr("905900000001", SSNMSC),
			CallingAddress: gtAddr("905304445566", SSNMSC),
			Data:           data[i*size : end],
			Segmentation:   &Segmentation{First: i == 0, Class1: false, Remaining: uint8(n - 1 - i), LocalRef: ref},
		}
		b, err := EncodeSCCP(msg, VariantITU)
		require.NoError(t, err)
		out = append(out, b)
	}
	return out
}

func feed(r *routingRig, frames ...[]byte) {
	for _, f := range frames {
		r.h.handleIncomingMessage(&m3ua.MTP3Message{OPC: stpPC, DPC: ourPC, SI: 3, PayloadData: f})
	}
}

func delivered(t *testing.T, r *routingRig) *SccpMessage {
	t.Helper()
	select {
	case m := <-r.h.TcapIncomingChannel():
		return m
	case <-time.After(time.Second):
		t.Fatal("nothing delivered")
		return nil
	}
}

func nothingDelivered(t *testing.T, r *routingRig) {
	t.Helper()
	select {
	case m := <-r.h.TcapIncomingChannel():
		t.Fatalf("unexpected delivery of %d octets", len(m.Data))
	default:
	}
}

func payload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func TestXUDT_SegmentationRoundTrip(t *testing.T) {
	segs := segments(t, payload(300), 2, 0xABCDEF, true)
	got, err := ParseSCCP(segs[0], VariantITU)
	require.NoError(t, err)
	require.NotNil(t, got.Segmentation)
	assert.Equal(t, Segmentation{First: true, Remaining: 1, LocalRef: 0xABCDEF}, *got.Segmentation)
	assert.Equal(t, uint8(15), got.HopCounter)
	assert.True(t, got.ReturnOnError)
	assert.Equal(t, "905304445566", got.CallingAddress.GlobalTitle.Address)
	assert.Len(t, got.Data, 150)
}

func TestXUDT_OptionalPart(t *testing.T) {
	b := segments(t, payload(10), 1, 1, false)[0]
	// Insert an unknown optional parameter (importance, 0x12) before the
	// segmentation parameter: unknown parameters are skipped.
	i := bytes.Index(b, []byte{paramSegmentation, 4})
	withUnknown := append(append(append([]byte{}, b[:i]...), 0x12, 1, 0x03), b[i:]...)
	got, err := ParseSCCP(withUnknown, VariantITU)
	require.NoError(t, err)
	require.NotNil(t, got.Segmentation)

	_, err = ParseSCCP(b[:len(b)-2], VariantITU)
	assert.Error(t, err, "truncated optional part")
}

func TestReassembly_InOrder(t *testing.T) {
	r := newRoutingRig(t, true)
	data := payload(600)
	segs := segments(t, data, 3, 7, false)
	feed(r, segs[0], segs[1])
	nothingDelivered(t, r)
	feed(r, segs[2])
	m := delivered(t, r)
	assert.Equal(t, data, m.Data)
	assert.Nil(t, m.Segmentation)
	assert.Equal(t, ClassBasicConnectionless, m.ProtocolClass, "the class the user asked for (C bit)")
	nothingDelivered(t, r)
}

// The SCCP worker pool can reorder segments the network sent in sequence.
func TestReassembly_ReorderedInternally(t *testing.T) {
	r := newRoutingRig(t, true)
	data := payload(600)
	segs := segments(t, data, 3, 8, false)
	feed(r, segs[2], segs[0], segs[1])
	assert.Equal(t, data, delivered(t, r).Data)
}

func TestReassembly_SingleSegment(t *testing.T) {
	r := newRoutingRig(t, true)
	feed(r, segments(t, payload(100), 1, 9, false)...)
	assert.Len(t, delivered(t, r).Data, 100)
}

func expectReturn(t *testing.T, r *routingRig, cause ReturnCause) *SccpMessage {
	t.Helper()
	label, msg := r.sent(t)
	assert.Equal(t, uint32(stpPC), label.DPC)
	assert.Equal(t, MsgTypeXUDTS, msg.MessageType)
	assert.Equal(t, cause, msg.ReturnCause)
	return msg
}

func TestReassembly_Errors(t *testing.T) {
	t.Run("duplicate segment", func(t *testing.T) {
		r := newRoutingRig(t, true)
		segs := segments(t, payload(600), 3, 10, true)
		feed(r, segs[0], segs[1], segs[1])
		ret := expectReturn(t, r, CauseErrorInMessageTransport)
		assert.Len(t, ret.Data, 200, "the first segment's data is returned")
		nothingDelivered(t, r)
	})
	t.Run("first segment for a busy process", func(t *testing.T) {
		r := newRoutingRig(t, true)
		segs := segments(t, payload(600), 3, 11, true)
		feed(r, segs[0], segs[0])
		expectReturn(t, r, CauseErrorInMessageTransport)
	})
	t.Run("remaining does not fit the first segment", func(t *testing.T) {
		r := newRoutingRig(t, true)
		a := segments(t, payload(600), 3, 12, true)
		b := segments(t, payload(800), 4, 12, true) // b[0] says 3 remaining
		feed(r, a[0], b[1])                         // b[1] says 2 remaining, as many as a[0]
		expectReturn(t, r, CauseErrorInMessageTransport)
	})
	t.Run("longer than 3952 octets", func(t *testing.T) {
		// Not reachable through valid encodings: the one-octet optional-part
		// pointer keeps a segment within 247 octets (Q.713 Table 19), and 16 x
		// 247 is exactly the 3952 limit. The check guards the reassembler
		// itself, so it is driven directly.
		r := newRoutingRig(t, true)
		var failure *reassemblyFailure
		for i := 0; i < 16 && failure == nil; i++ {
			_, failure = r.h.reassemble(&SccpMessage{
				MessageType: MsgTypeXUDT, ReturnOnError: true,
				CallingAddress: gtAddr("905304445566", SSNMSC), Data: payload(255),
				Segmentation: &Segmentation{First: i == 0, Remaining: uint8(15 - i), LocalRef: 13},
			}, stpPC)
		}
		require.NotNil(t, failure)
		assert.Equal(t, CauseCannotReassemble, failure.cause)
	})
	t.Run("no return without the return option", func(t *testing.T) {
		r := newRoutingRig(t, true)
		segs := segments(t, payload(600), 3, 14, false)
		feed(r, segs[0], segs[0])
		r.nothingSent(t)
	})
}

func TestReassembly_Timeout(t *testing.T) {
	r := newRoutingRig(t, true)
	segs := segments(t, payload(600), 3, 15, true)
	feed(r, segs[0])
	r.h.reassembler.mu.Lock()
	var p *reassembly
	for _, v := range r.h.reassembler.processes {
		p = v
	}
	r.h.reassembler.mu.Unlock()
	require.NotNil(t, p)

	r.h.expireReassembly(p) // what T(reass) does
	expectReturn(t, r, CauseErrorInMessageTransport)
	feed(r, segs[1], segs[2]) // too late: no process, and no first segment
	nothingDelivered(t, r)
}

func TestReassembly_Bounded(t *testing.T) {
	r := newRoutingRig(t, true)
	for i := 0; i < maxReassemblies; i++ {
		feed(r, segments(t, payload(400), 2, uint32(1000+i), false)[0])
	}
	feed(r, segments(t, payload(400), 2, 99, true)[0])
	expectReturn(t, r, CauseNetworkCongestion)
}
