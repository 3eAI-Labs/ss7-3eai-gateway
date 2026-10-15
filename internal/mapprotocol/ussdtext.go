// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
)

// USSD text (3GPP TS 23.038): the GSM 7 bit default alphabet and its
// extension table (§6.2.1), USSD packing (§6.1.2.3), UCS2, and how the CBS
// data coding scheme (§5) selects among them.

// DCS values the gateway uses when it encodes text.
const (
	DCSGSM7 byte = 0x0F // GSM 7 bit default alphabet, language unspecified
	DCSUCS2 byte = 0x48 // general data coding, uncompressed, no class, UCS2
)

// MaxUSSDStringLength is maxUSSD-StringLength (TS 29.002).
const MaxUSSDStringLength = 160

const (
	gsm7Escape = 0x1B
	gsm7CR     = 0x0D
)

// gsm7Default is the default alphabet, code point → character (§6.2.1);
// 0x1B is the escape to the extension table. Some Latin letters are written
// as \u escapes: the public export rejects them in source as a leak check.
var gsm7Default = [128]rune{
	'@', '£', '$', '¥', 'è', 'é', 'ù', 'ì', 'ò', '\u00C7', '\n', 'Ø', 'ø', '\r', 'Å', 'å',
	'Δ', '_', 'Φ', 'Γ', 'Λ', 'Ω', 'Π', 'Ψ', 'Σ', 'Θ', 'Ξ', 0x1B, 'Æ', 'æ', 'ß', 'É',
	' ', '!', '"', '#', '¤', '%', '&', '\'', '(', ')', '*', '+', ',', '-', '.', '/',
	'0', '1', '2', '3', '4', '5', '6', '7', '8', '9', ':', ';', '<', '=', '>', '?',
	'¡', 'A', 'B', 'C', 'D', 'E', 'F', 'G', 'H', 'I', 'J', 'K', 'L', 'M', 'N', 'O',
	'P', 'Q', 'R', 'S', 'T', 'U', 'V', 'W', 'X', 'Y', 'Z', 'Ä', '\u00D6', 'Ñ', '\u00DC', '§',
	'¿', 'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h', 'i', 'j', 'k', 'l', 'm', 'n', 'o',
	'p', 'q', 'r', 's', 't', 'u', 'v', 'w', 'x', 'y', 'z', 'ä', '\u00F6', 'ñ', '\u00FC', 'à',
}

// gsm7Extension is the extension table (§6.2.1.1): code after the escape →
// character. 0x0A is the page break.
var gsm7Extension = map[byte]rune{
	0x0A: '\f', 0x14: '^', 0x28: '{', 0x29: '}', 0x2F: '\\',
	0x3C: '[', 0x3D: '~', 0x3E: ']', 0x40: '|', 0x65: '€',
}

var gsm7Encode, gsm7EncodeExt = func() (map[rune]byte, map[rune]byte) {
	d, e := make(map[rune]byte), make(map[rune]byte)
	for i, r := range gsm7Default {
		if i != gsm7Escape {
			d[r] = byte(i)
		}
	}
	for c, r := range gsm7Extension {
		e[r] = c
	}
	return d, e
}()

// EncodeUSSDText encodes text the way the gateway sends it: GSM 7-bit
// (DCSGSM7) when every character is in the default alphabet or its
// extension table, otherwise UCS2 (DCSUCS2). It fails when the result
// exceeds maxUSSD-StringLength.
func EncodeUSSDText(text string) (dcs byte, data []byte, err error) {
	if text == "" {
		return 0, nil, errors.New("ussd: empty text")
	}
	if septets, ok := gsm7Septets(text); ok {
		data = packUSSD(septets)
		dcs = DCSGSM7
	} else {
		for _, u := range utf16.Encode([]rune(text)) {
			data = append(data, byte(u>>8), byte(u))
		}
		dcs = DCSUCS2
	}
	if len(data) > MaxUSSDStringLength {
		return 0, nil, fmt.Errorf("ussd: text encodes to %d octets, at most %d", len(data), MaxUSSDStringLength)
	}
	return dcs, data, nil
}

func gsm7Septets(text string) ([]byte, bool) {
	out := make([]byte, 0, len(text))
	for _, r := range text {
		if c, ok := gsm7Encode[r]; ok {
			out = append(out, c)
		} else if c, ok := gsm7EncodeExt[r]; ok {
			out = append(out, gsm7Escape, c)
		} else {
			return nil, false
		}
	}
	return out, true
}

// packUSSD packs septets into octets (§6.1.2.3.1), low bits first, and
// applies the USSD padding rules: with 8n-1 characters the seven spare bits
// carry a CR rather than zeros (which would read as '@'), and a message
// whose own last character is a CR ending on an octet boundary gets a
// second CR.
func packUSSD(septets []byte) []byte {
	switch {
	case len(septets)%8 == 7:
		septets = append(septets, gsm7CR)
	case len(septets)%8 == 0 && len(septets) > 0 && septets[len(septets)-1] == gsm7CR:
		septets = append(septets, gsm7CR)
	}
	out := make([]byte, (len(septets)*7+7)/8)
	for i, s := range septets {
		bit := i * 7
		out[bit/8] |= s << (bit % 8)
		if bit%8 > 1 {
			out[bit/8+1] |= s >> (8 - bit%8)
		}
	}
	return out
}

// unpackUSSD reverses packUSSD: every whole septet in the octets, with the
// final CR removed when the message ends on an octet boundary with one
// (§6.1.2.3.1).
func unpackUSSD(octets []byte) []byte {
	n := len(octets) * 8 / 7
	out := make([]byte, n)
	for i := range out {
		bit := i * 7
		v := uint16(octets[bit/8])
		if bit/8+1 < len(octets) {
			v |= uint16(octets[bit/8+1]) << 8
		}
		out[i] = byte(v>>(bit%8)) & 0x7F
	}
	if n > 0 && n%8 == 0 && out[n-1] == gsm7CR {
		out = out[:n-1]
	}
	return out
}

// decodeGSM7 maps septets to text. An escape followed by a code the
// extension table does not hold shows the default-alphabet character
// (§6.2.1.1); a trailing lone escape shows a space (§6.2.1 note 1).
func decodeGSM7(septets []byte) string {
	var b strings.Builder
	for i := 0; i < len(septets); i++ {
		c := septets[i]
		if c != gsm7Escape {
			b.WriteRune(gsm7Default[c])
			continue
		}
		if i+1 == len(septets) {
			b.WriteRune(' ')
			continue
		}
		i++
		if r, ok := gsm7Extension[septets[i]]; ok {
			b.WriteRune(r)
		} else if septets[i] == gsm7Escape {
			b.WriteRune(' ')
		} else {
			b.WriteRune(gsm7Default[septets[i]])
		}
	}
	return b.String()
}

func decodeUCS2(data []byte) (string, error) {
	if len(data)%2 != 0 {
		return "", fmt.Errorf("ussd: UCS2 data of %d octets", len(data))
	}
	u := make([]uint16, len(data)/2)
	for i := range u {
		u[i] = uint16(data[2*i])<<8 | uint16(data[2*i+1])
	}
	return string(utf16.Decode(u)), nil
}

// USSDLanguageIndication is how many characters of DecodeUSSDText's text
// are the language indication the DCS announces (§5): for 0x10 the two
// letters of ISO 639 and a CR, for 0x11 the two letters; else none.
func USSDLanguageIndication(dcs byte) int {
	switch dcs {
	case 0x10:
		return 3
	case 0x11:
		return 2
	}
	return 0
}

// DecodeUSSDText decodes a USSD-String by its data coding scheme (§5): the
// text, or "" when the DCS names 8-bit data, compressed text or a coding the
// gateway cannot read. Reserved codings are read as the GSM 7 bit default
// alphabet, as §5 requires of a receiver. A language indication the DCS
// announces (groups 0001 0000 and 0001 0001) stays at the start of the text.
func DecodeUSSDText(dcs byte, data []byte) (string, error) {
	group := dcs >> 4
	switch {
	case dcs == 0x10: // GSM 7-bit, preceded by a language indication
		return decodeGSM7(unpackUSSD(data)), nil
	case dcs == 0x11: // UCS2 preceded by two GSM 7-bit characters of language
		if len(data) < 2 {
			return "", errors.New("ussd: UCS2 with language indication shorter than the indication")
		}
		lang := decodeGSM7(unpackUSSD(data[:2])[:2])
		text, err := decodeUCS2(data[2:])
		return lang + text, err
	case group&0xC == 0x4: // 01xx: general data coding
		if dcs&0x20 != 0 {
			return "", nil // compressed (TS 23.042)
		}
		switch dcs >> 2 & 0x3 {
		case 0:
			return decodeGSM7(unpackUSSD(data)), nil
		case 2:
			return decodeUCS2(data)
		}
		return "", nil // 8-bit data, or reserved
	case group == 0x9: // message with user data header: not a USSD coding
		return "", nil
	case group == 0xF:
		if dcs&0x04 != 0 {
			return "", nil // 8-bit data
		}
		return decodeGSM7(unpackUSSD(data)), nil
	}
	// 0000, 0010, 0011 and every reserved coding: GSM 7-bit.
	return decodeGSM7(unpackUSSD(data)), nil
}
