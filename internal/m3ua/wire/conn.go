// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package wire

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ishidawataru/sctp"
)

// ppidM3UA is the SCTP Payload Protocol Identifier of M3UA (RFC 4666 §7.1),
// in host order: the SCTP library converts it to network order itself
// (SCTPWrite: htonl). Converting it here too sent 0x03000000.
const ppidM3UA = 3

// Streams the association asks for: stream 0 for management, the others
// for DATA (RFC 4666 §1.4.7).
const defaultStreams = 17

// readBufferSize holds any M3UA message: SCCP user data is at most a few
// kilobytes; SCTP keeps message boundaries.
const readBufferSize = 64 * 1024

// Config configures an association.
type Config struct {
	// ASPIdentifier, when non-zero, is sent in ASP Up.
	ASPIdentifier uint32
	// RoutingContext, when non-zero, is sent in ASP Active and DATA.
	RoutingContext uint32
	// NetworkAppearance, when non-zero, is sent in DATA.
	NetworkAppearance uint32
	// TrafficMode is sent in ASP Active (TrafficModeOverride or
	// TrafficModeLoadshare); zero leaves it out.
	TrafficMode uint32
	// HandshakeTimeout bounds the wait for ASP Up Ack (ASP), or for the
	// whole ASP Up / ASP Active exchange (SG). Zero means 2 s.
	HandshakeTimeout time.Duration
	// ActiveAckTimeout bounds the wait for ASP Active Ack; zero means
	// HandshakeTimeout.
	ActiveAckTimeout time.Duration
	// HeartbeatInterval, when non-zero, sends BEAT that often and drops the
	// association when nothing has arrived for two intervals (RFC 4666
	// §4.3.4.6: any message shows the peer alive). SCTP has its own
	// heartbeat, so M3UA's is optional (§3.5.5).
	HeartbeatInterval time.Duration
	// OnManagement, when set, sees every message other than DATA, BEAT and
	// BEAT Ack (NTFY, ERR, SSNM, …), on the reading goroutine.
	OnManagement func(*Message)
}

func (c *Config) handshakeTimeout() time.Duration {
	if c.HandshakeTimeout > 0 {
		return c.HandshakeTimeout
	}
	return 2 * time.Second
}

// Conn is one M3UA association. Its messages are read by one goroutine at
// a time (ReadData), in the order the peer sent them; writes may come from
// any goroutine.
type Conn struct {
	sc         *sctp.SCTPConn
	cfg        Config
	outStreams uint16

	wmu       sync.Mutex
	closeOnce sync.Once
	closed    chan struct{}

	buf          []byte
	lastReceived atomic.Int64     // unix nanoseconds
	lastInfo     *sctp.SndRcvInfo // set when the socket reports it (SCTP_EVENT_DATA_IO)
	lastRaw      []byte
}

func newConn(sc *sctp.SCTPConn, cfg Config) (*Conn, error) {
	c := &Conn{sc: sc, cfg: cfg, closed: make(chan struct{}), buf: make([]byte, readBufferSize)}
	st, err := sc.GetStatus()
	if err != nil {
		_ = sc.Close()
		return nil, fmt.Errorf("m3ua: SCTP status: %w", err)
	}
	c.outStreams = st.Ostreams
	if c.outStreams < 2 {
		// Stream 0 is for management and DATA never goes on it (RFC 4666
		// §1.4.7): one stream would make an ACTIVE link that sends nothing.
		_ = sc.Close()
		return nil, fmt.Errorf("m3ua: the association has %d outbound stream(s); DATA needs at least 2", c.outStreams)
	}
	return c, nil
}

// dialRetries bounds the immediate re-dials after EISCONN: a blocking SCTP
// connect interrupted by a signal is restarted on the same socket, and if
// the association came up meanwhile the restarted call reports EISCONN and
// the socket is closed with it.
const dialRetries = 3

// Dial connects to an SG as an ASP and brings the association to ASP-ACTIVE:
// ASP Up → ASP Up Ack, then ASP Active → ASP Active Ack (RFC 4666 §4.3).
func Dial(laddr, raddr *sctp.SCTPAddr, cfg Config) (*Conn, error) {
	var sc *sctp.SCTPConn
	var err error
	for attempt := 1; ; attempt++ {
		sc, err = sctp.DialSCTPExt("sctp", laddr, raddr,
			sctp.InitMsg{NumOstreams: defaultStreams, MaxInstreams: defaultStreams})
		if err == nil || !errors.Is(err, syscall.EISCONN) || attempt == dialRetries {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("m3ua: SCTP connect: %w", err)
	}
	c, err := newConn(sc, cfg)
	if err != nil {
		return nil, err
	}
	if err := c.aspHandshake(); err != nil {
		_ = c.Close()
		return nil, err
	}
	c.startHeartbeat()
	return c, nil
}

var errHandshakeRefused = errors.New("m3ua: SG refused the ASP")

func (c *Conn) aspHandshake() error {
	up := &Message{Class: ClassASPSM, Type: TypeASPUP}
	if c.cfg.ASPIdentifier != 0 {
		up.Params = append(up.Params, u32Param(TagASPIdentifier, c.cfg.ASPIdentifier))
	}
	if err := c.write(up, 0); err != nil {
		return err
	}
	if err := c.awaitWithin(c.cfg.handshakeTimeout(), ClassASPSM, TypeASPUPAck); err != nil {
		return fmt.Errorf("m3ua: ASP Up: %w", err)
	}
	ac := &Message{Class: ClassASPTM, Type: TypeASPAC}
	if c.cfg.TrafficMode != 0 {
		ac.Params = append(ac.Params, u32Param(TagTrafficModeType, c.cfg.TrafficMode))
	}
	if c.cfg.RoutingContext != 0 {
		ac.Params = append(ac.Params, u32Param(TagRoutingContext, c.cfg.RoutingContext))
	}
	if err := c.write(ac, 0); err != nil {
		return err
	}
	acTimeout := c.cfg.ActiveAckTimeout
	if acTimeout <= 0 {
		acTimeout = c.cfg.handshakeTimeout()
	}
	if err := c.awaitWithin(acTimeout, ClassASPTM, TypeASPACAck); err != nil {
		return fmt.Errorf("m3ua: ASP Active: %w", err)
	}
	return nil
}

// awaitWithin is await bounded by d: the timer closes the association,
// which ends the blocked read.
func (c *Conn) awaitWithin(d time.Duration, class, typ uint8) error {
	timer := time.AfterFunc(d, func() { _ = c.Close() })
	err := c.await(class, typ)
	if !timer.Stop() {
		return fmt.Errorf("no acknowledgement within %s", d)
	}
	return err
}

// await reads until the given message arrives. Management messages on the
// way (NTFY) are passed to OnManagement; an ERR refuses the handshake.
func (c *Conn) await(class, typ uint8) error {
	for {
		m, err := c.readMessage()
		if err != nil {
			return err
		}
		switch {
		case m.Class == class && m.Type == typ:
			return nil
		case m.Class == ClassMGMT && m.Type == TypeERR:
			code, _ := m.Uint32(TagErrorCode)
			c.management(m)
			return fmt.Errorf("%w: ERR %#02x", errHandshakeRefused, code)
		case m.Class == ClassASPSM && m.Type == TypeBEAT:
			_ = c.write(&Message{Class: ClassASPSM, Type: TypeBEATAck, Params: m.Params}, 0)
		default:
			c.management(m)
		}
	}
}

// ReadData returns the next DATA's Protocol Data, in the order the peer
// sent them. On the way it answers BEAT, notes BEAT Ack, hands other
// messages to OnManagement, and answers what it cannot accept with an ERR.
// It returns an error when the association ends.
func (c *Conn) ReadData() (*ProtocolData, error) {
	for {
		m, err := c.readMessage()
		if err != nil {
			var pe *ProtocolError
			if errors.As(err, &pe) {
				c.replyError(pe.Code)
				continue
			}
			return nil, err
		}
		switch {
		case m.Class == ClassTransfer && m.Type == TypeDATA:
			pd, err := m.ProtocolData()
			if err != nil {
				c.replyError(ErrProtocolError)
				continue
			}
			return pd, nil
		case m.Class == ClassTransfer:
			c.replyError(ErrUnsupportedMessageType)
		case m.Class == ClassASPSM && m.Type == TypeBEAT:
			_ = c.write(&Message{Class: ClassASPSM, Type: TypeBEATAck, Params: m.Params}, 0)
		case m.Class == ClassASPSM && m.Type == TypeBEATAck:
		case m.Class == ClassASPSM && m.Type == TypeASPDN:
			_ = c.write(&Message{Class: ClassASPSM, Type: TypeASPDNAck}, 0)
			return nil, errors.New("m3ua: peer sent ASP Down")
		case m.Class == ClassMGMT && m.Type == TypeNTFY:
			c.management(m)
			// "Alternate ASP Active": another ASP took the traffic in
			// Override mode, and this one "MUST consider itself now in the
			// ASP-INACTIVE state" (RFC 4666 §4.3.4.3). The association is
			// dropped, so the link shows down and the supervisor brings it
			// back to ASP-ACTIVE.
			if st, ok := m.Uint32(TagStatus); ok && st == statusAlternateASPActive {
				return nil, errors.New("m3ua: NTFY Alternate ASP Active: this ASP is inactive")
			}
		case m.Class == ClassMGMT, m.Class == ClassSSNM, m.Class == ClassASPSM, m.Class == ClassASPTM:
			c.management(m)
		default:
			c.replyError(ErrUnsupportedMessageClass)
		}
	}
}

// statusAlternateASPActive is the NTFY Status of type Other (2), info
// Alternate ASP Active (2) (RFC 4666 §3.8.2).
const statusAlternateASPActive = 2<<16 | 2

// readMessage reads one SCTP message and parses it. Any message counts as
// the peer being alive.
func (c *Conn) readMessage() (*Message, error) {
	n, info, err := c.sc.SCTPRead(c.buf)
	if err != nil {
		if c.isClosed() {
			return nil, net.ErrClosed
		}
		return nil, err
	}
	if n == 0 {
		return nil, io.EOF // the peer shut the association down
	}
	c.lastReceived.Store(time.Now().UnixNano())
	c.lastInfo = info
	c.lastRaw = c.buf[:n]
	return Parse(c.buf[:n])
}

// replyError answers what could not be accepted with an ERR carrying the
// first 40 octets of it (RFC 4666 §3.8.1) — except an ERR: "Error messages
// MUST NOT be generated in response to other Error messages".
func (c *Conn) replyError(code uint32) {
	raw := c.lastRaw
	if len(raw) >= 4 && raw[2] == ClassMGMT && raw[3] == TypeERR {
		return
	}
	if len(raw) > 40 {
		raw = raw[:40]
	}
	_ = c.write(NewError(code, append([]byte(nil), raw...)), 0)
}

func (c *Conn) management(m *Message) {
	if c.cfg.OnManagement != nil {
		c.cfg.OnManagement(m)
	}
}

// WriteData sends a DATA on stream, which must be a data stream
// (1..MaxStream): DATA never goes on stream 0 (RFC 4666 §1.4.7).
func (c *Conn) WriteData(pd *ProtocolData, stream uint16) (int, error) {
	if stream == 0 || stream >= c.outStreams {
		return 0, fmt.Errorf("m3ua: DATA on stream %d, data streams are 1..%d", stream, c.MaxStream())
	}
	m := NewData(pd, DataOptions{NetworkAppearance: c.cfg.NetworkAppearance, RoutingContext: c.cfg.RoutingContext})
	b := m.Marshal()
	if err := c.writeRaw(b, stream); err != nil {
		return 0, err
	}
	return len(b), nil
}

// MaxStream is the highest outbound stream number; streams 1..MaxStream
// carry DATA.
func (c *Conn) MaxStream() uint16 {
	if c.outStreams == 0 {
		return 0
	}
	return c.outStreams - 1
}

func (c *Conn) write(m *Message, stream uint16) error {
	return c.writeRaw(m.Marshal(), stream)
}

func (c *Conn) writeRaw(b []byte, stream uint16) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.isClosed() {
		return net.ErrClosed
	}
	_, err := c.sc.SCTPWrite(b, &sctp.SndRcvInfo{Stream: stream, PPID: ppidM3UA})
	return err
}

// startHeartbeat sends BEAT every HeartbeatInterval and closes the
// association when nothing has arrived for two intervals.
func (c *Conn) startHeartbeat() {
	iv := c.cfg.HeartbeatInterval
	if iv <= 0 {
		return
	}
	c.lastReceived.Store(time.Now().UnixNano())
	go func() {
		t := time.NewTicker(iv)
		defer t.Stop()
		for {
			select {
			case <-c.closed:
				return
			case <-t.C:
				if time.Since(time.Unix(0, c.lastReceived.Load())) > 2*iv {
					_ = c.Close()
					return
				}
				_ = c.write(&Message{Class: ClassASPSM, Type: TypeBEAT}, 0)
			}
		}
	}()
}

func (c *Conn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// Close closes the association. Safe to call more than once and from any
// goroutine; a blocked ReadData returns.
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closed)
		err = c.sc.Close()
	})
	return err
}

// LocalAddr and RemoteAddr are the association's addresses.
func (c *Conn) LocalAddr() net.Addr  { return c.sc.LocalAddr() }
func (c *Conn) RemoteAddr() net.Addr { return c.sc.RemoteAddr() }

// Listener accepts associations as an SG — for test peers: an SG side
// that answers ASP Up and ASP Active and carries DATA.
type Listener struct {
	ln  *sctp.SCTPListener
	cfg Config
}

// Listen listens for ASPs on laddr.
func Listen(laddr *sctp.SCTPAddr, cfg Config) (*Listener, error) {
	ln, err := sctp.ListenSCTPExt("sctp", laddr, sctp.InitMsg{NumOstreams: defaultStreams, MaxInstreams: defaultStreams})
	if err != nil {
		return nil, err
	}
	return &Listener{ln: ln, cfg: cfg}, nil
}

// Accept waits for the next association. The ASP handshake is not part of
// it — call Handshake, typically in a goroutine of its own, so one ASP that
// never completes cannot hold back the next.
func (l *Listener) Accept() (*Conn, error) {
	sc, err := l.ln.AcceptSCTP()
	if err != nil {
		return nil, err
	}
	return newConn(sc, l.cfg)
}

// Close stops listening.
func (l *Listener) Close() error { return l.ln.Close() }

// Addr is the listening address.
func (l *Listener) Addr() net.Addr { return l.ln.Addr() }

// Handshake answers the ASP's ASP Up and ASP Active (RFC 4666 §4.3), within
// HandshakeTimeout; the association can then carry DATA.
func (c *Conn) Handshake() error {
	timer := time.AfterFunc(c.cfg.handshakeTimeout(), func() { _ = c.Close() })
	defer timer.Stop()
	for step := 0; step < 2; {
		m, err := c.readMessage()
		if err != nil {
			return err
		}
		switch {
		case m.Class == ClassASPSM && m.Type == TypeASPUP && step == 0:
			if err := c.write(&Message{Class: ClassASPSM, Type: TypeASPUPAck}, 0); err != nil {
				return err
			}
			step++
		case m.Class == ClassASPTM && m.Type == TypeASPAC && step == 1:
			var params []Param
			for _, p := range m.Params {
				if p.Tag == TagTrafficModeType || p.Tag == TagRoutingContext {
					params = append(params, p)
				}
			}
			if err := c.write(&Message{Class: ClassASPTM, Type: TypeASPACAck, Params: params}, 0); err != nil {
				return err
			}
			step++
		case m.Class == ClassASPSM && m.Type == TypeBEAT:
			_ = c.write(&Message{Class: ClassASPSM, Type: TypeBEATAck, Params: m.Params}, 0)
		case m.Class == ClassMGMT && m.Type == TypeERR:
			return errors.New("m3ua: ASP sent ERR during the handshake")
		default:
			c.replyError(ErrUnexpectedMessage)
		}
	}
	return nil
}
