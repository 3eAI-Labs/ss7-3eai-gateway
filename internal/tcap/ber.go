// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package tcap

import (
	"encoding/asn1"
	"errors"
	"fmt"
)

// The TCAP message codec (ITU-T Q.773 §3–4): transaction portion, dialogue
// portion (Q.773 §3.2 DialoguePDUs inside EXTERNAL) and component portion.
//
// Encoding follows Q.773 §4.1.1: definite lengths, short form below 128
// octets and the minimal long form above. A component's parameter is the
// operation's argument, result or error parameter itself — "ANY DEFINED BY
// operationCode", its own tag and length included — so it is carried
// verbatim in both directions: the bytes the MAP codec produces are the
// bytes on the wire, and the bytes on the wire are what the MAP codec reads.
//
// Decoding accepts every BER length: ParseMessage first rewrites the
// indefinite form (Q.773 §4.1.2.3) and non-minimal long forms into the
// definite, minimal lengths encoding/asn1 reads (berlength.go), the
// component parameters included.

// Tags of the TCAP message types and transaction portion (Q.773 §3.1).
const (
	tagOTID             = 8  // [APPLICATION 8] OrigTransactionID
	tagDTID             = 9  // [APPLICATION 9] DestTransactionID
	tagPAbortCause      = 10 // [APPLICATION 10] P-AbortCause
	tagDialoguePortion  = 11 // [APPLICATION 11] DialoguePortion
	tagComponentPortion = 12 // [APPLICATION 12] ComponentPortion

	// Component tags (context-specific, constructed).
	tagInvoke              = 1
	tagReturnResultLast    = 2
	tagReturnError         = 3
	tagReject              = 4
	tagReturnResultNotLast = 7

	// Dialogue PDUs (Q.773 §3.2.1), [APPLICATION n].
	tagAARQ = 0
	tagAARE = 1
	tagABRT = 4

	tagUserInformation = 30 // [30] IMPLICIT SEQUENCE OF EXTERNAL

	tagExternal = 8 // [UNIVERSAL 8] EXTERNAL (X.208)
)

// Abstract syntax names of the dialogue portion (Q.773 §3.2).
var (
	dialogueAsID    = asn1.ObjectIdentifier{0, 0, 17, 773, 1, 1, 1}
	uniDialogueAsID = asn1.ObjectIdentifier{0, 0, 17, 773, 1, 2, 1}
)

// protocolVersion1 is protocol-version [0] IMPLICIT BIT STRING {version1(0)}
// with version1 set: one bit, seven unused.
var protocolVersion1 = []byte{0x07, 0x80}

// EncodeMessage encodes a TCAP message (Q.773).
func EncodeMessage(msg *Message) ([]byte, error) {
	if msg == nil {
		return nil, ErrInvalidMessage
	}
	var fields [][]byte
	tid := func(tag int, id []byte, name string) error {
		if len(id) < 1 || len(id) > 4 {
			return fmt.Errorf("%s of %d octets, want 1..4", name, len(id))
		}
		fields = append(fields, ber(asn1.ClassApplication, tag, false, id))
		return nil
	}
	var tag int
	switch msg.MessageType {
	case MsgTypeUnidirectional:
		// The gateway sends no UNI; one would need an AUDT under
		// uniDialogue-as-id (Q.773 §3.2.2) and a component portion.
		if msg.DialoguePortion != nil || len(msg.Components) == 0 {
			return nil, errors.New("UNI with a dialogue portion or without components is not supported")
		}
		tag = 1
	case MsgTypeBegin:
		tag = 2
		if err := tid(tagOTID, msg.OriginatingTID, "OTID"); err != nil {
			return nil, err
		}
	case MsgTypeEnd:
		tag = 4
		if err := tid(tagDTID, msg.DestinationTID, "DTID"); err != nil {
			return nil, err
		}
	case MsgTypeContinue:
		tag = 5
		if err := tid(tagOTID, msg.OriginatingTID, "OTID"); err != nil {
			return nil, err
		}
		if err := tid(tagDTID, msg.DestinationTID, "DTID"); err != nil {
			return nil, err
		}
	case MsgTypeAbort:
		tag = 7
		if err := tid(tagDTID, msg.DestinationTID, "DTID"); err != nil {
			return nil, err
		}
		if msg.PAbort {
			cause := 0
			if msg.AbortCause != nil {
				cause = int(*msg.AbortCause)
			}
			fields = append(fields, ber(asn1.ClassApplication, tagPAbortCause, false, intContent(cause)))
		}
	default:
		return nil, fmt.Errorf("unsupported message type: %v", msg.MessageType)
	}
	// A P-ABORT carries its cause instead of a dialogue portion.
	if dlg := msg.DialoguePortion; dlg != nil && !(msg.MessageType == MsgTypeAbort && msg.PAbort) {
		b, err := encodeDialoguePortion(msg.MessageType, dlg)
		if err != nil {
			return nil, err
		}
		fields = append(fields, b)
	}
	if len(msg.Components) > 0 {
		if msg.MessageType == MsgTypeAbort {
			return nil, errors.New("ABORT carries no components")
		}
		var comps []byte
		for _, c := range msg.Components {
			b, err := encodeComponent(c)
			if err != nil {
				return nil, err
			}
			comps = append(comps, b...)
		}
		fields = append(fields, ber(asn1.ClassApplication, tagComponentPortion, true, comps))
	}
	return ber(asn1.ClassApplication, tag, true, concat(fields...)), nil
}

// encodeDialoguePortion encodes [APPLICATION 11] EXTERNAL carrying the
// dialogue PDU the message type calls for: AARQ in a BEGIN, ABRT in a
// TC-U-ABORT that does not refuse a context, otherwise an AARE — accepted,
// or reject-permanent with application-context-name-not-supported when
// Rejected (Q.774: the answer to an AARQ naming a context the responder
// does not support).
func encodeDialoguePortion(mt MessageType, dlg *DialoguePortion) ([]byte, error) {
	var pdu []byte
	userInfo := func() []byte {
		if len(dlg.UserInformation) == 0 {
			return nil
		}
		return ber(asn1.ClassContextSpecific, tagUserInformation, true, dlg.UserInformation)
	}
	acn := func() ([]byte, error) {
		if dlg.ApplicationContext == nil || len(dlg.ApplicationContext.OID) < 2 {
			return nil, errors.New("dialogue portion without an application context name")
		}
		oid, err := asn1.Marshal(asn1.ObjectIdentifier(dlg.ApplicationContext.OID))
		if err != nil {
			return nil, fmt.Errorf("application context name: %w", err)
		}
		return ber(asn1.ClassContextSpecific, 1, true, oid), nil
	}
	switch {
	case mt == MsgTypeBegin:
		name, err := acn()
		if err != nil {
			return nil, err
		}
		pdu = ber(asn1.ClassApplication, tagAARQ, true, concat(
			ber(asn1.ClassContextSpecific, 0, false, protocolVersion1), name, userInfo()))
	case mt == MsgTypeAbort && (dlg.ApplicationContext == nil || !dlg.Rejected):
		source := 0 // dialogue-service-user
		if dlg.AbortSourceProvider {
			source = 1
		}
		pdu = ber(asn1.ClassApplication, tagABRT, true, concat(
			ber(asn1.ClassContextSpecific, 0, false, intContent(source)), userInfo()))
	default:
		name, err := acn()
		if err != nil {
			return nil, err
		}
		result, diagnostic := 0, 0 // accepted, dialogue-service-user null
		if dlg.Rejected {
			result, diagnostic = 1, 2 // reject-permanent, application-context-name-not-supported
		}
		pdu = ber(asn1.ClassApplication, tagAARE, true, concat(
			ber(asn1.ClassContextSpecific, 0, false, protocolVersion1),
			name,
			ber(asn1.ClassContextSpecific, 2, true, berInteger(result)),
			ber(asn1.ClassContextSpecific, 3, true,
				ber(asn1.ClassContextSpecific, 1, true, berInteger(diagnostic))),
			userInfo()))
	}
	asID, _ := asn1.Marshal(dialogueAsID)
	external := ber(asn1.ClassUniversal, tagExternal, true, concat(
		asID, ber(asn1.ClassContextSpecific, 0, true, pdu))) // single-ASN1-type [0]
	return ber(asn1.ClassApplication, tagDialoguePortion, true, external), nil
}

func encodeComponent(c *Component) ([]byte, error) {
	if c == nil {
		return nil, errors.New("nil component")
	}
	// The parameter is one ASN.1 value with its own tag and length; anything
	// else would put a malformed component on the wire.
	if len(c.Parameter) > 0 {
		var rv asn1.RawValue
		if rest, err := asn1.Unmarshal(c.Parameter, &rv); err != nil || len(rest) > 0 {
			return nil, fmt.Errorf("parameter is not one BER element (%d octets)", len(c.Parameter))
		}
	}
	invokeID := func() ([]byte, error) {
		if c.InvokeID == nil {
			return nil, errors.New("component without an invoke ID")
		}
		if *c.InvokeID < -128 || *c.InvokeID > 127 {
			return nil, fmt.Errorf("invoke ID %d outside -128..127", *c.InvokeID)
		}
		return berInteger(int(*c.InvokeID)), nil
	}
	switch c.Type {
	case CompTypeInvoke:
		id, err := invokeID()
		if err != nil {
			return nil, err
		}
		if c.OperationCode == nil {
			return nil, errors.New("invoke without an operation code")
		}
		var linked []byte
		if c.LinkedID != nil {
			if *c.LinkedID < -128 || *c.LinkedID > 127 {
				return nil, fmt.Errorf("linked ID %d outside -128..127", *c.LinkedID)
			}
			linked = ber(asn1.ClassContextSpecific, 0, false, intContent(int(*c.LinkedID)))
		}
		return ber(asn1.ClassContextSpecific, tagInvoke, true,
			concat(id, linked, berInteger(int(*c.OperationCode)), c.Parameter)), nil
	case CompTypeReturnResult:
		id, err := invokeID()
		if err != nil {
			return nil, err
		}
		// The result SEQUENCE is present only with a parameter: an
		// operation whose result is empty sends the invoke ID alone.
		var result []byte
		if len(c.Parameter) > 0 {
			if c.OperationCode == nil {
				return nil, errors.New("return result with a parameter but no operation code")
			}
			result = ber(asn1.ClassUniversal, asn1.TagSequence, true,
				concat(berInteger(int(*c.OperationCode)), c.Parameter))
		}
		tag := tagReturnResultLast
		if c.NotLast {
			tag = tagReturnResultNotLast
		}
		return ber(asn1.ClassContextSpecific, tag, true, concat(id, result)), nil
	case CompTypeReturnError:
		id, err := invokeID()
		if err != nil {
			return nil, err
		}
		if c.ErrorCode == nil {
			return nil, errors.New("return error without an error code")
		}
		return ber(asn1.ClassContextSpecific, tagReturnError, true,
			concat(id, berInteger(int(*c.ErrorCode)), c.Parameter)), nil
	case CompTypeReject:
		id := ber(asn1.ClassUniversal, asn1.TagNull, false, nil) // not-derivable
		if c.InvokeID != nil {
			var err error
			if id, err = invokeID(); err != nil {
				return nil, err
			}
		}
		var problem []byte
		if p := c.ProblemCode; p != nil {
			if p.Type > ProblemTypeReturnError {
				return nil, fmt.Errorf("reject problem type %d", p.Type)
			}
			problem = ber(asn1.ClassContextSpecific, int(p.Type), false, intContent(int(p.Code)))
		} else {
			problem = ber(asn1.ClassContextSpecific, int(ProblemTypeGeneral), false, intContent(0))
		}
		return ber(asn1.ClassContextSpecific, tagReject, true, concat(id, problem)), nil
	}
	return nil, fmt.Errorf("unsupported component type %#x", uint8(c.Type))
}

// ParseMessage decodes a TCAP message (Q.773). It never panics.
func ParseMessage(data []byte) (*Message, error) {
	data, err := definiteLengths(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}
	var outer asn1.RawValue
	rest, err := asn1.Unmarshal(data, &outer)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}
	if len(rest) > 0 {
		return nil, fmt.Errorf("%w: %d octets after the message", ErrInvalidMessage, len(rest))
	}
	if outer.Class != asn1.ClassApplication || !outer.IsCompound {
		return nil, fmt.Errorf("%w: not a TCAP message type", ErrInvalidMessage)
	}
	msg := &Message{}
	switch outer.Tag {
	case 1:
		msg.MessageType = MsgTypeUnidirectional
	case 2:
		msg.MessageType = MsgTypeBegin
	case 4:
		msg.MessageType = MsgTypeEnd
	case 5:
		msg.MessageType = MsgTypeContinue
	case 7:
		msg.MessageType = MsgTypeAbort
	default:
		return nil, fmt.Errorf("%w: message type [APPLICATION %d]", ErrInvalidMessage, outer.Tag)
	}
	fields, err := children(outer.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}
	for _, f := range fields {
		if f.Class != asn1.ClassApplication {
			return nil, fmt.Errorf("%w: unexpected element class %d", ErrInvalidMessage, f.Class)
		}
		switch {
		case f.Tag == tagOTID && !f.IsCompound:
			if len(f.Bytes) < 1 || len(f.Bytes) > 4 {
				return nil, fmt.Errorf("%w: OTID of %d octets", ErrInvalidMessage, len(f.Bytes))
			}
			msg.OriginatingTID = f.Bytes
		case f.Tag == tagDTID && !f.IsCompound:
			if len(f.Bytes) < 1 || len(f.Bytes) > 4 {
				return nil, fmt.Errorf("%w: DTID of %d octets", ErrInvalidMessage, len(f.Bytes))
			}
			msg.DestinationTID = f.Bytes
		case f.Tag == tagPAbortCause && !f.IsCompound:
			v, err := intValue(f.Bytes)
			if err != nil || v < 0 || v > 255 {
				return nil, fmt.Errorf("%w: P-AbortCause", ErrInvalidMessage)
			}
			cause := AbortCause(v)
			msg.AbortCause, msg.PAbort = &cause, true
		case f.Tag == tagDialoguePortion && f.IsCompound:
			if msg.DialoguePortion, err = parseDialoguePortion(f.Bytes); err != nil {
				return nil, fmt.Errorf("%w: dialogue portion: %v", ErrInvalidMessage, err)
			}
		case f.Tag == tagComponentPortion && f.IsCompound:
			comps, err := children(f.Bytes)
			if err != nil {
				return nil, fmt.Errorf("%w: component portion: %v", ErrInvalidMessage, err)
			}
			for i, raw := range comps {
				c, err := parseComponent(raw)
				if err != nil {
					return nil, fmt.Errorf("%w: component %d: %v", ErrInvalidMessage, i+1, err)
				}
				msg.Components = append(msg.Components, c)
			}
		default:
			return nil, fmt.Errorf("%w: unexpected [APPLICATION %d]", ErrInvalidMessage, f.Tag)
		}
	}
	switch msg.MessageType {
	case MsgTypeBegin:
		if msg.OriginatingTID == nil {
			return nil, fmt.Errorf("%w: BEGIN without OTID", ErrInvalidMessage)
		}
	case MsgTypeContinue:
		if msg.OriginatingTID == nil || msg.DestinationTID == nil {
			return nil, fmt.Errorf("%w: CONTINUE without OTID and DTID", ErrInvalidMessage)
		}
	case MsgTypeEnd, MsgTypeAbort:
		if msg.DestinationTID == nil {
			return nil, fmt.Errorf("%w: message without DTID", ErrInvalidMessage)
		}
	}
	return msg, nil
}

// parseDialoguePortion reads the EXTERNAL inside [APPLICATION 11]: the
// direct reference, then the dialogue PDU as single-ASN1-type [0].
func parseDialoguePortion(b []byte) (*DialoguePortion, error) {
	ext, err := onlyOne(b)
	if err != nil {
		return nil, err
	}
	if ext.Class != asn1.ClassUniversal || ext.Tag != tagExternal || !ext.IsCompound {
		return nil, errors.New("not an EXTERNAL")
	}
	parts, err := children(ext.Bytes)
	if err != nil {
		return nil, err
	}
	dlg := &DialoguePortion{}
	var asID asn1.ObjectIdentifier
	var pdu *asn1.RawValue
	for i := range parts {
		p := parts[i]
		switch {
		case p.Class == asn1.ClassUniversal && p.Tag == asn1.TagOID:
			if _, err := asn1.Unmarshal(p.FullBytes, &asID); err != nil {
				return nil, fmt.Errorf("direct-reference: %w", err)
			}
		case p.Class == asn1.ClassContextSpecific && p.Tag == 0 && p.IsCompound:
			inner, err := onlyOne(p.Bytes)
			if err != nil {
				return nil, fmt.Errorf("single-ASN1-type: %w", err)
			}
			pdu = &inner
		}
	}
	if !asID.Equal(dialogueAsID) && !asID.Equal(uniDialogueAsID) {
		// User information in some other abstract syntax (a 1988 abort):
		// nothing TCAP reads.
		return dlg, nil
	}
	if pdu == nil || pdu.Class != asn1.ClassApplication || !pdu.IsCompound {
		return nil, errors.New("no dialogue PDU")
	}
	elems, err := children(pdu.Bytes)
	if err != nil {
		return nil, err
	}
	dlg.PDU = DialoguePDUType(pdu.Tag)
	for _, e := range elems {
		if e.Class != asn1.ClassContextSpecific {
			continue
		}
		switch e.Tag {
		case 0:
			if pdu.Tag == tagABRT {
				v, err := intValue(e.Bytes)
				if err != nil {
					return nil, fmt.Errorf("abort-source: %w", err)
				}
				dlg.AbortSourceProvider = v == 1
			}
		case 1:
			oid, err := onlyOne(e.Bytes)
			if err != nil {
				return nil, fmt.Errorf("application-context-name: %w", err)
			}
			var ac asn1.ObjectIdentifier
			if _, err := asn1.Unmarshal(oid.FullBytes, &ac); err != nil {
				return nil, fmt.Errorf("application-context-name: %w", err)
			}
			dlg.ApplicationContext = &ApplicationContext{OID: []int(ac)}
		case 2:
			if pdu.Tag == tagAARE {
				r, err := onlyOne(e.Bytes)
				if err != nil {
					return nil, fmt.Errorf("result: %w", err)
				}
				v, err := intValue(r.Bytes)
				if err != nil {
					return nil, fmt.Errorf("result: %w", err)
				}
				dlg.Rejected = v != 0
			}
		case tagUserInformation:
			dlg.UserInformation = e.Bytes
		}
	}
	return dlg, nil
}

func parseComponent(raw asn1.RawValue) (*Component, error) {
	if raw.Class != asn1.ClassContextSpecific || !raw.IsCompound {
		return nil, errors.New("not a component")
	}
	c := &Component{}
	switch raw.Tag {
	case tagInvoke:
		c.Type = CompTypeInvoke
	case tagReturnResultLast:
		c.Type = CompTypeReturnResult
	case tagReturnResultNotLast:
		c.Type, c.NotLast = CompTypeReturnResult, true
	case tagReturnError:
		c.Type = CompTypeReturnError
	case tagReject:
		c.Type = CompTypeReject
	default:
		return nil, fmt.Errorf("component type [%d]", raw.Tag)
	}
	elems, err := children(raw.Bytes)
	if err != nil {
		return nil, err
	}
	if len(elems) == 0 {
		return nil, errors.New("empty component")
	}
	// Every component starts with the invoke ID; a Reject's may be NULL.
	first := elems[0]
	switch {
	case isUniversalPrim(first, asn1.TagInteger):
		v, err := intValue(first.Bytes)
		if err != nil || v < -128 || v > 127 {
			return nil, errors.New("invoke ID outside -128..127")
		}
		id := int32(v)
		c.InvokeID = &id
	case c.Type == CompTypeReject && isUniversalPrim(first, asn1.TagNull):
	default:
		return nil, errors.New("invoke ID missing")
	}
	rest := elems[1:]
	code := func(name string) (*int32, error) {
		if len(rest) == 0 {
			return nil, fmt.Errorf("%s missing", name)
		}
		e := rest[0]
		if !isUniversalPrim(e, asn1.TagInteger) {
			// A global (OBJECT IDENTIFIER) value is not used by MAP or CAP.
			return nil, fmt.Errorf("%s is not a local value", name)
		}
		v, err := intValue(e.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		rest = rest[1:]
		code := int32(v)
		return &code, nil
	}
	parameter := func() error {
		switch len(rest) {
		case 0:
		case 1:
			c.Parameter = rest[0].FullBytes
		default:
			return errors.New("more than one parameter")
		}
		return nil
	}
	switch c.Type {
	case CompTypeInvoke:
		if len(rest) > 0 && rest[0].Class == asn1.ClassContextSpecific && rest[0].Tag == 0 && !rest[0].IsCompound {
			v, err := intValue(rest[0].Bytes)
			if err != nil || v < -128 || v > 127 {
				return nil, errors.New("linked ID outside -128..127")
			}
			linked := int32(v)
			c.LinkedID = &linked
			rest = rest[1:]
		}
		if c.OperationCode, err = code("operation code"); err != nil {
			return nil, err
		}
		if err := parameter(); err != nil {
			return nil, err
		}
	case CompTypeReturnResult:
		switch len(rest) {
		case 0: // no result
		case 1:
			if !isUniversalCons(rest[0], asn1.TagSequence) {
				return nil, errors.New("result is not a SEQUENCE")
			}
			if rest, err = children(rest[0].Bytes); err != nil {
				return nil, err
			}
			if c.OperationCode, err = code("operation code"); err != nil {
				return nil, err
			}
			if err := parameter(); err != nil {
				return nil, err
			}
		default:
			return nil, errors.New("return result with more than one result")
		}
	case CompTypeReturnError:
		if c.ErrorCode, err = code("error code"); err != nil {
			return nil, err
		}
		if err := parameter(); err != nil {
			return nil, err
		}
	case CompTypeReject:
		if len(rest) != 1 || rest[0].Class != asn1.ClassContextSpecific || rest[0].IsCompound || rest[0].Tag > 3 {
			return nil, errors.New("reject problem missing or malformed")
		}
		v, err := intValue(rest[0].Bytes)
		if err != nil || v < 0 || v > 255 {
			return nil, errors.New("reject problem code")
		}
		c.ProblemCode = &ProblemCode{Type: uint8(rest[0].Tag), Code: uint8(v)}
	}
	return c, nil
}

// ----- BER helpers -----------------------------------------------------------

// ber encodes one element with a definite length, short or minimal long
// form (Q.773 §4.1.1).
func ber(class, tag int, compound bool, content []byte) []byte {
	b, err := asn1.Marshal(asn1.RawValue{Class: class, Tag: tag, IsCompound: compound, Bytes: content})
	if err != nil {
		panic(fmt.Sprintf("ber: %v", err)) // only for negative tags, never used here
	}
	return b
}

func berInteger(v int) []byte {
	return ber(asn1.ClassUniversal, asn1.TagInteger, false, intContent(v))
}

// intContent is the minimal two's-complement content of v.
func intContent(v int) []byte {
	b, err := asn1.Marshal(int64(v))
	if err != nil {
		panic(fmt.Sprintf("intContent: %v", err)) // cannot fail for an int64
	}
	return b[2:] // tag and one-octet length: at most 8 content octets
}

// intValue reads a two's-complement INTEGER of at most 4 octets.
func intValue(b []byte) (int, error) {
	if len(b) == 0 || len(b) > 4 {
		return 0, fmt.Errorf("integer of %d octets", len(b))
	}
	v := int(int8(b[0]))
	for _, o := range b[1:] {
		v = v<<8 | int(o)
	}
	return v, nil
}

func children(b []byte) ([]asn1.RawValue, error) {
	var out []asn1.RawValue
	for len(b) > 0 {
		var rv asn1.RawValue
		rest, err := asn1.Unmarshal(b, &rv)
		if err != nil {
			return nil, err
		}
		out = append(out, rv)
		b = rest
	}
	return out, nil
}

func onlyOne(b []byte) (asn1.RawValue, error) {
	c, err := children(b)
	if err != nil {
		return asn1.RawValue{}, err
	}
	if len(c) != 1 {
		return asn1.RawValue{}, fmt.Errorf("%d elements, want 1", len(c))
	}
	return c[0], nil
}

func isUniversalPrim(rv asn1.RawValue, tag int) bool {
	return rv.Class == asn1.ClassUniversal && rv.Tag == tag && !rv.IsCompound
}

func isUniversalCons(rv asn1.RawValue, tag int) bool {
	return rv.Class == asn1.ClassUniversal && rv.Tag == tag && rv.IsCompound
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
