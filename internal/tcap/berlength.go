// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package tcap

import (
	"errors"
	"fmt"
)

// A peer may send any BER length X.690 §8.1.3 allows: the indefinite form
// for a constructed element (Q.773 §4.1.2.3 names it for TCAP), and a long
// form with more octets than the length needs. encoding/asn1, which every
// decoder here is built on, reads DER lengths only — definite and minimal —
// and refuses both. definiteLengths rewrites what arrives into those
// lengths before anything reads it, so the TCAP, MAP and CAP decoders all
// see one form: the content is unchanged, element by element, and only the
// length octets (and an indefinite element's end-of-contents) differ.
//
// Lengths only: a constructed OCTET STRING or BIT STRING (X.690 §8.7.3,
// §8.6.3) stays as it is, because under an IMPLICIT tag nothing in the
// encoding says that a constructed element is a string, and the decoders
// refuse it.

// maxBERNesting bounds how deep definiteLengths follows constructed
// elements, so that a message of nested indefinite headers cannot run the
// recursion as deep as the message is long. Real messages nest far less:
// the 138 hex frames in this repository's tests, whole TCAP messages among
// them, nest at most 8 deep.
const maxBERNesting = 64

// definiteLengths returns b, a sequence of BER elements, with every length
// in the definite, minimal form. It returns b itself when it is already in
// that form.
func definiteLengths(b []byte) ([]byte, error) {
	canonical := true
	for rest := b; len(rest) > 0; {
		n, ok, err := checkElement(rest, 0)
		if err != nil {
			return nil, err
		}
		canonical = canonical && ok
		rest = rest[n:]
	}
	if canonical {
		return b, nil
	}
	out := make([]byte, 0, len(b)+8)
	for rest := b; len(rest) > 0; {
		var n int
		var err error
		if out, n, err = appendElement(out, rest, 0); err != nil {
			return nil, err
		}
		rest = rest[n:]
	}
	return out, nil
}

// checkElement reads the element at the start of b and reports its size in
// b and whether all its lengths, nested ones included, are already definite
// and minimal.
func checkElement(b []byte, depth int) (int, bool, error) {
	h, err := readHeader(b, depth)
	if err != nil {
		return 0, false, err
	}
	if h.indefinite {
		n, err := indefiniteEnd(b, h, depth)
		return n, false, err
	}
	end := h.size + h.length
	ok := h.minimal
	if h.compound {
		for pos := h.size; pos < end; {
			n, childOK, err := checkElement(b[pos:end], depth+1)
			if err != nil {
				return end, h.minimal, nil // opaque: see appendElement
			}
			ok = ok && childOK
			pos += n
		}
	}
	return end, ok, nil
}

// indefiniteEnd returns the size in b of an indefinite-length element:
// its header, its elements and the end-of-contents octets.
func indefiniteEnd(b []byte, h berHeader, depth int) (int, error) {
	pos := h.size
	for {
		if endOfContents(b[pos:]) {
			return pos + 2, nil
		}
		n, _, err := checkElement(b[pos:], depth+1)
		if err != nil {
			return 0, err
		}
		pos += n
	}
}

// appendElement appends the element at the start of b to out with definite,
// minimal lengths throughout, and returns how many octets of b it took.
func appendElement(out, b []byte, depth int) ([]byte, int, error) {
	h, err := readHeader(b, depth)
	if err != nil {
		return nil, 0, err
	}
	out = append(out, b[:h.identifier]...)
	if !h.compound {
		end := h.size + h.length
		return append(appendLength(out, h.length), b[h.size:end]...), end, nil
	}
	var content []byte
	pos, end := h.size, h.size+h.length
	for {
		if h.indefinite && endOfContents(b[pos:]) {
			pos += 2
			break
		}
		if !h.indefinite && pos == end {
			break
		}
		limit := len(b)
		if !h.indefinite {
			limit = end
		}
		var n int
		if content, n, err = appendElement(content, b[pos:limit], depth+1); err != nil {
			if h.indefinite {
				return nil, 0, err // its end is past what cannot be read
			}
			// A definite element whose content does not read as elements
			// goes on as it came: its length bounds it, and what is wrong
			// inside is for the decoder of that element to answer — a
			// malformed operation argument is a Reject of that component
			// (Q.774), not a message that cannot be parsed.
			return append(appendLength(out, h.length), b[h.size:end]...), end, nil
		}
		pos += n
	}
	return append(appendLength(out, len(content)), content...), pos, nil
}

// berHeader is an element's identifier and length octets as read.
type berHeader struct {
	identifier int  // octets of the identifier
	size       int  // octets of identifier and length together
	compound   bool // constructed
	indefinite bool // length octet 0x80; length is then 0
	length     int  // content octets, for a definite length
	minimal    bool // the definite length is in its shortest form
}

// readHeader reads the identifier and length octets at the start of b and,
// for a definite length, checks that the content is all in b.
func readHeader(b []byte, depth int) (berHeader, error) {
	if depth > maxBERNesting {
		return berHeader{}, fmt.Errorf("BER: nested more than %d deep", maxBERNesting)
	}
	if len(b) == 0 {
		return berHeader{}, errors.New("BER: element missing")
	}
	h := berHeader{identifier: 1, compound: b[0]&0x20 != 0}
	if b[0]&0x1f == 0x1f { // high tag number: more octets, bit 8 set on all but the last
		for {
			if h.identifier == len(b) {
				return berHeader{}, errors.New("BER: identifier truncated")
			}
			last := b[h.identifier]&0x80 == 0
			h.identifier++
			if last {
				break
			}
		}
	}
	if h.identifier == len(b) {
		return berHeader{}, errors.New("BER: length missing")
	}
	first := b[h.identifier]
	h.size = h.identifier + 1
	switch {
	case first < 0x80:
		h.length, h.minimal = int(first), true
	case first == 0x80:
		if !h.compound {
			return berHeader{}, errors.New("BER: indefinite length on a primitive element")
		}
		h.indefinite = true
		return h, nil
	case first == 0xff:
		return berHeader{}, errors.New("BER: reserved length octet 0xff")
	default:
		n := int(first & 0x7f)
		if len(b)-h.size < n {
			return berHeader{}, errors.New("BER: length truncated")
		}
		for _, o := range b[h.size : h.size+n] {
			if h.length > len(b) { // stop before it can overflow; it cannot fit anyway
				return berHeader{}, errors.New("BER: length beyond the message")
			}
			h.length = h.length<<8 | int(o)
		}
		h.size += n
		h.minimal = h.length >= 0x80 && b[h.identifier+1] != 0
	}
	if h.length > len(b)-h.size {
		return berHeader{}, errors.New("BER: length beyond the message")
	}
	return h, nil
}

// endOfContents reports whether b starts with the end-of-contents octets
// that close an indefinite length (X.690 §8.1.5).
func endOfContents(b []byte) bool {
	return len(b) >= 2 && b[0] == 0 && b[1] == 0
}

// appendLength appends n as a definite length, short form below 128 and
// minimal long form above (X.690 §10.1).
func appendLength(out []byte, n int) []byte {
	if n < 0x80 {
		return append(out, byte(n))
	}
	var buf [8]byte
	i := len(buf)
	for ; n > 0; n >>= 8 {
		i--
		buf[i] = byte(n)
	}
	out = append(out, 0x80|byte(len(buf)-i))
	return append(out, buf[i:]...)
}
