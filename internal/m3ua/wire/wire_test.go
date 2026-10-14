// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package wire

import (
	"bytes"
	"encoding/hex"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ishidawataru/sctp"
)

// Frames built by hand from RFC 4666 §3 (common header, TLV parameters
// padded to four octets, the Protocol Data layout of §3.3.1).

func frame(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var frames = map[string]struct {
	hex string
	msg *Message
}{
	"ASP Up with ASP Identifier 1": {
		"01 00 03 01 00 00 00 10  00 11 00 08 00 00 00 01",
		&Message{Class: ClassASPSM, Type: TypeASPUP, Params: []Param{{TagASPIdentifier, []byte{0, 0, 0, 1}}}},
	},
	"ASP Active, Loadshare, Routing Context 1": {
		"01 00 04 01 00 00 00 18  00 0b 00 08 00 00 00 02  00 06 00 08 00 00 00 01",
		&Message{Class: ClassASPTM, Type: TypeASPAC, Params: []Param{
			{TagTrafficModeType, []byte{0, 0, 0, 2}}, {TagRoutingContext, []byte{0, 0, 0, 1}}}},
	},
	"DATA, Routing Context 1, three octets of user data, padded": {
		"01 00 01 01 00 00 00 24  00 06 00 08 00 00 00 01" +
			" 02 10 00 13  00 00 00 01  00 00 00 02  03 02 00 05  01 02 03 00",
		NewData(&ProtocolData{OPC: 1, DPC: 2, SI: 3, NI: 2, MP: 0, SLS: 5, Data: []byte{1, 2, 3}}, DataOptions{RoutingContext: 1}),
	},
	"BEAT Ack echoing Heartbeat Data": {
		"01 00 03 06 00 00 00 10  00 09 00 06 ab cd 00 00",
		&Message{Class: ClassASPSM, Type: TypeBEATAck, Params: []Param{{TagHeartbeatData, []byte{0xab, 0xcd}}}},
	},
	"ERR Unexpected Message": {
		"01 00 00 00 00 00 00 10  00 0c 00 08 00 00 00 06",
		NewError(ErrUnexpectedMessage, nil),
	},
}

func TestFrames(t *testing.T) {
	for name, c := range frames {
		want := frame(t, c.hex)
		if got := c.msg.Marshal(); !bytes.Equal(got, want) {
			t.Errorf("%s: marshal\n got % X\nwant % X", name, got, want)
		}
		m, err := Parse(want)
		if err != nil || !reflect.DeepEqual(m, c.msg) {
			t.Errorf("%s: parse %+v, %v", name, m, err)
		}
	}
}

// RFC 4666 §3.1.4: a receiver should accept a message whose length leaves
// out the final padding.
func TestParseWithoutFinalPadding(t *testing.T) {
	b := frame(t, "01 00 01 01 00 00 00 23  00 06 00 08 00 00 00 01"+
		" 02 10 00 13  00 00 00 01  00 00 00 02  03 02 00 05  01 02 03")
	m, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	pd, err := m.ProtocolData()
	if err != nil || pd.SLS != 5 || !bytes.Equal(pd.Data, []byte{1, 2, 3}) {
		t.Fatalf("%+v %v", pd, err)
	}
}

func TestNotifyStatus(t *testing.T) {
	m, err := Parse(frame(t, "01 00 00 01 00 00 00 10  00 0d 00 08 00 01 00 03"))
	if err != nil || m.Class != ClassMGMT || m.Type != TypeNTFY {
		t.Fatalf("%+v %v", m, err)
	}
	if v, _ := m.Uint32(TagStatus); v != 0x00010003 { // AS-State_Change, AS-ACTIVE
		t.Fatalf("status %#x", v)
	}
}

func TestParseRejects(t *testing.T) {
	for name, h := range map[string]string{
		"short":                    "01 00 03 01 00 00",
		"version 2":                "02 00 03 01 00 00 00 08",
		"length beyond the data":   "01 00 03 01 00 00 00 10  00 11 00 08",
		"length under the header":  "01 00 03 01 00 00 00 04",
		"parameter length under 4": "01 00 03 01 00 00 00 0c  00 11 00 02",
		"parameter beyond message": "01 00 03 01 00 00 00 0c  00 11 00 08",
	} {
		if m, err := Parse(frame(t, h)); err == nil {
			t.Errorf("%s: parsed %+v", name, m)
		}
	}
	m := &Message{Class: ClassTransfer, Type: TypeDATA, Params: []Param{{TagProtocolData, make([]byte, 11)}}}
	if _, err := m.ProtocolData(); err == nil {
		t.Error("protocol data of 11 octets accepted")
	}
}

// Over a real SCTP association: the ASP handshake, then DATA in both
// directions, received in the order sent — the property the replaced
// library lost.
func TestAssociationKeepsOrder(t *testing.T) {
	addr := &sctp.SCTPAddr{IPAddrs: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}}
	ln, err := Listen(addr, Config{})
	if err != nil {
		t.Skipf("SCTP not available: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*sctp.SCTPAddr).Port

	sgCh := make(chan *Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			t.Error(err)
			close(sgCh)
			return
		}
		if err := c.Handshake(); err != nil {
			t.Error(err)
		}
		sgCh <- c
	}()
	var mgmt []*Message
	asp, err := Dial(nil, &sctp.SCTPAddr{IPAddrs: addr.IPAddrs, Port: port}, Config{
		ASPIdentifier: 1, RoutingContext: 1, TrafficMode: TrafficModeLoadshare,
		HandshakeTimeout: 2 * time.Second, OnManagement: func(m *Message) { mgmt = append(mgmt, m) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer asp.Close()
	sg := <-sgCh
	if sg == nil {
		t.Fatal("no SG association")
	}
	defer sg.Close()
	// The receiving side reports each message's stream and PPID.
	if err := sg.sc.SubscribeEvents(sctp.SCTP_EVENT_DATA_IO); err != nil {
		t.Fatal(err)
	}

	const n = 2000
	go func() {
		for i := 0; i < n; i++ {
			pd := &ProtocolData{OPC: 2, DPC: 1, SI: 3, NI: 2, SLS: uint8(i % 16), Data: []byte{byte(i >> 8), byte(i)}}
			if _, err := sg.WriteData(pd, 1+uint16(i%int(sg.MaxStream()))); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	// Per stream, SCTP keeps order; the reader keeps it too.
	last := map[uint8]int{}
	for i := 0; i < n; i++ {
		pd, err := asp.ReadData()
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		seq := int(pd.Data[0])<<8 | int(pd.Data[1])
		if prev, ok := last[pd.SLS]; ok && seq <= prev {
			t.Fatalf("SLS %d: %d after %d", pd.SLS, seq, prev)
		}
		last[pd.SLS] = seq
	}

	// And the other way, with a BEAT on the way that the SG answers.
	if err := asp.write(&Message{Class: ClassASPSM, Type: TypeBEAT, Params: []Param{{TagHeartbeatData, []byte{7}}}}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := asp.WriteData(&ProtocolData{OPC: 1, DPC: 2, SI: 3, SLS: 1, Data: []byte{9}}, 1); err != nil {
		t.Fatal(err)
	}
	pd, err := sg.ReadData()
	if err != nil || pd.Data[0] != 9 {
		t.Fatalf("SG read %+v %v", pd, err)
	}
	// RFC 4666 §7.1: PPID 3; §1.4.7: DATA not on stream 0.
	if info := sg.lastInfo; info == nil || info.PPID != 3 || info.Stream != 1 {
		t.Fatalf("DATA arrived with %+v, want PPID 3 on stream 1", info)
	}
	if _, err := asp.WriteData(&ProtocolData{}, 0); err == nil {
		t.Error("DATA on stream 0 accepted")
	}
}

func TestDialHandshakeTimeout(t *testing.T) {
	addr := &sctp.SCTPAddr{IPAddrs: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}}
	ln, err := Listen(addr, Config{})
	if err != nil {
		t.Skipf("SCTP not available: %v", err)
	}
	defer ln.Close()
	go func() { // accepts, never answers
		c, err := ln.Accept()
		if err == nil {
			time.Sleep(time.Second)
			c.Close()
		}
	}()
	start := time.Now()
	_, err = Dial(nil, &sctp.SCTPAddr{IPAddrs: addr.IPAddrs, Port: ln.Addr().(*sctp.SCTPAddr).Port},
		Config{HandshakeTimeout: 300 * time.Millisecond})
	if err == nil || time.Since(start) > 900*time.Millisecond {
		t.Fatalf("err %v after %s", err, time.Since(start))
	}
}

// What an SG sends that the ASP does not accept is answered with an ERR
// carrying its first 40 octets; an ERR is not answered; NTFY Alternate ASP
// Active ends the association (RFC 4666 §3.8.1, §4.3.4.3).
func TestASPAnswersAndAlternateASP(t *testing.T) {
	addr := &sctp.SCTPAddr{IPAddrs: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}}
	ln, err := Listen(addr, Config{})
	if err != nil {
		t.Skipf("SCTP not available: %v", err)
	}
	defer ln.Close()
	sgCh := make(chan *Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil && c.Handshake() == nil {
			sgCh <- c
		}
	}()
	var seen []*Message
	asp, err := Dial(nil, &sctp.SCTPAddr{IPAddrs: addr.IPAddrs, Port: ln.Addr().(*sctp.SCTPAddr).Port},
		Config{OnManagement: func(m *Message) { seen = append(seen, m) }})
	if err != nil {
		t.Fatal(err)
	}
	defer asp.Close()
	sg := <-sgCh
	defer sg.Close()

	unknownType := &Message{Class: ClassTransfer, Type: 9}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(sg.write(unknownType, 0))
	must(sg.write(NewError(ErrProtocolError, nil), 0)) // not to be answered
	must(sg.write(&Message{Class: ClassMGMT, Type: TypeNTFY, Params: []Param{u32Param(TagStatus, statusAlternateASPActive)}}, 0))
	if _, err := asp.ReadData(); err == nil {
		t.Fatal("the ASP stayed active after Alternate ASP Active")
	}
	m, err := sg.readMessage()
	if err != nil || m.Class != ClassMGMT || m.Type != TypeERR {
		t.Fatalf("SG got %+v %v, want ERR", m, err)
	}
	if code, _ := m.Uint32(TagErrorCode); code != ErrUnsupportedMessageType {
		t.Errorf("error code %#x, want unsupported message type", code)
	}
	if diag, _ := m.Get(TagDiagnosticInfo); !bytes.Equal(diag, unknownType.Marshal()) {
		t.Errorf("diagnostic % X, want the offending message", diag)
	}
	if len(seen) != 2 || seen[0].Type != TypeERR || seen[1].Type != TypeNTFY {
		t.Errorf("management seen: %+v", seen)
	}
}
