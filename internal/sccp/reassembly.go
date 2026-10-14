// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package sccp

import (
	"bytes"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Reassembly of segmented XUDT messages (Q.714 §4.1.1.2).
//
// A reassembly process is identified by the calling party address, the MTP
// routing information (the OPC) and the segmentation local reference. The
// gateway never segments what it sends — it uses the TCAP empty Begin
// (TS 29.002 §C.2.1) — but a peer may segment what it sends to us.
//
// One deviation, invisible on the wire: segments are placed by their
// "remaining segments" number instead of being required in arrival order,
// because the SCCP worker pool can reorder messages the network delivered in
// sequence. A duplicate, a number that does not fit the first segment, or a
// missing segment at T(reass) is still an error in message transport.

const (
	// reassemblyTimeout is T(reass): 10 to 20 s (Q.714 Annex, timers).
	reassemblyTimeout = 15 * time.Second
	// maxReassembledData is the longest message segmentation can carry
	// (Q.714 §4.1.1.1.1).
	maxReassembledData = 3952
	// maxReassemblies bounds concurrent reassembly processes: each holds
	// buffered data for a peer that has not finished sending.
	maxReassemblies = 1000
)

type reassemblyKey struct {
	calling string // encoded calling party address
	opc     uint32
	ref     uint32
}

type reassembly struct {
	key           reassemblyKey
	first         *SccpMessage     // nil until the first segment arrives
	parts         map[uint8][]byte // user data by "remaining segments" number
	size          int
	returnOnError bool
	earliest      *SccpMessage // the first segment received, returned on error if the first never came
	timer         *time.Timer
}

type reassembler struct {
	mu        sync.Mutex
	processes map[reassemblyKey]*reassembly
}

func newReassembler() *reassembler {
	return &reassembler{processes: make(map[reassemblyKey]*reassembly)}
}

// reassemblyFailure is a message to return (when its sender asked) and why.
type reassemblyFailure struct {
	msg   *SccpMessage
	cause ReturnCause
}

// reassemble takes one segment. It returns the whole message once every
// segment is in, a failure to report, or neither while segments are missing.
func (h *Handler) reassemble(seg *SccpMessage, opc uint32) (*SccpMessage, *reassemblyFailure) {
	s := seg.Segmentation
	if s.First && s.Remaining == 0 {
		return withoutSegmentation(seg, seg.Data), nil // a single segment
	}
	key := reassemblyKey{
		calling: string(encodeAddress(seg.CallingAddress, h.config.NetworkVariant)),
		opc:     opc,
		ref:     s.LocalRef,
	}

	r := h.reassembler
	r.mu.Lock()
	defer r.mu.Unlock()

	p := r.processes[key]
	if p == nil {
		if len(r.processes) >= maxReassemblies {
			return nil, &reassemblyFailure{seg, CauseNetworkCongestion} // no buffer space (Q.714 §4.1.1.2.3.4)
		}
		p = &reassembly{key: key, parts: make(map[uint8][]byte), earliest: seg}
		p.timer = time.AfterFunc(reassemblyTimeout, func() { h.expireReassembly(p) })
		r.processes[key] = p
	}

	fail := func(cause ReturnCause) (*SccpMessage, *reassemblyFailure) {
		p.timer.Stop()
		delete(r.processes, key)
		ret := p.first
		if ret == nil {
			ret = p.earliest
		}
		if !p.returnOnError && !seg.ReturnOnError {
			ret = nil
		}
		return nil, &reassemblyFailure{ret, cause}
	}

	if s.First {
		if p.first != nil {
			return fail(CauseErrorInMessageTransport) // first segment for a busy process
		}
		p.first = seg
		for rem := range p.parts {
			if rem >= s.Remaining {
				return fail(CauseErrorInMessageTransport)
			}
		}
	} else if p.first != nil && s.Remaining >= p.first.Segmentation.Remaining {
		return fail(CauseErrorInMessageTransport)
	}
	if _, dup := p.parts[s.Remaining]; dup {
		return fail(CauseErrorInMessageTransport)
	}
	p.parts[s.Remaining] = seg.Data
	p.size += len(seg.Data)
	p.returnOnError = p.returnOnError || seg.ReturnOnError

	if p.size > maxReassembledData {
		return fail(CauseCannotReassemble)
	}
	if p.first == nil {
		return nil, nil
	}
	total := int(p.first.Segmentation.Remaining) + 1
	// Q.714 §4.1.1.2.1: the message is at most the first segment's length
	// times the number of segments.
	if p.size > len(p.first.Data)*total {
		return fail(CauseErrorInMessageTransport)
	}
	if len(p.parts) < total {
		return nil, nil
	}

	var whole bytes.Buffer
	for rem := total - 1; rem >= 0; rem-- {
		whole.Write(p.parts[uint8(rem)])
	}
	p.timer.Stop()
	delete(r.processes, key)
	return withoutSegmentation(p.first, whole.Bytes()), nil
}

// expireReassembly is T(reass): the process is discarded and, if asked for,
// the message returned (Q.714 §4.1.1.2.1).
func (h *Handler) expireReassembly(p *reassembly) {
	r := h.reassembler
	r.mu.Lock()
	if r.processes[p.key] != p {
		r.mu.Unlock()
		return
	}
	delete(r.processes, p.key)
	r.mu.Unlock()

	h.metrics.Errors.Add(1)
	h.logger.Warn("SCCP reassembly timed out", zap.Int("segments_received", len(p.parts)))
	ret := p.first
	if ret == nil {
		ret = p.earliest
	}
	if p.returnOnError {
		ret.ReturnOnError = true
		h.sendUDTS(ret, CauseErrorInMessageTransport, p.key.opc)
	}
}

// withoutSegmentation is the reassembled message as its user sent it: the
// first segment's addresses, the whole data, the protocol class the user
// asked for (the segments themselves always travel as class 1).
func withoutSegmentation(first *SccpMessage, data []byte) *SccpMessage {
	m := *first
	m.Data = data
	m.ProtocolClass = ClassBasicConnectionless
	if first.Segmentation.Class1 {
		m.ProtocolClass = ClassInSequenceConnectionless
	}
	m.Segmentation = nil
	return &m
}
