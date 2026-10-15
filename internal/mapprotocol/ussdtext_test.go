// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"bytes"
	"strings"
	"testing"
)

// Packed octets worked out by hand from TS 23.038 §6.1.2.3.1's bit layout
// (character n's bit a is its most significant; bit 0 is sent first).
func TestUSSDPacking(t *testing.T) {
	for _, c := range []struct {
		text string
		hex  string
	}{
		// 2A 31 32 33 23: 35 bits, five zero pad bits.
		{"*123#", "AA 98 6C 36 02"},
		// Seven characters: the seven spare bits carry CR (0001101), the
		// layout's last octet "0 0 0 1 1 0 1 7a".
		{"ABCDEFG", "41 E1 90 58 34 1E 1B"},
		// Eight characters fill seven octets exactly.
		{"ABCDEFGH", "41 E1 90 58 34 1E 91"},
		// Extension table: '€' is escape 1B then 65.
		{"€", "9B 32"},
	} {
		dcs, got, err := EncodeUSSDText(c.text)
		if err != nil || dcs != DCSGSM7 {
			t.Errorf("%q: dcs %#x err %v", c.text, dcs, err)
			continue
		}
		if want := unhex(t, c.hex); !bytes.Equal(got, want) {
			t.Errorf("%q: packed % X, want % X", c.text, got, want)
		}
		back, err := DecodeUSSDText(DCSGSM7, got)
		if err != nil || back != c.text {
			t.Errorf("%q: decoded back as %q, %v", c.text, back, err)
		}
	}
}

// A message whose own last character is a CR ending on an octet boundary
// gets a second CR; the receiver removes one (§6.1.2.3.1).
func TestUSSDTrailingCR(t *testing.T) {
	text := "ABCDEFG\r" // eight characters, the last a CR
	_, packed, err := EncodeUSSDText(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(packed) != 8 { // nine septets: 63 bits
		t.Fatalf("packed into %d octets, want 8", len(packed))
	}
	if back, _ := DecodeUSSDText(DCSGSM7, packed); back != text+"\r" {
		// Nine septets do not end on an octet boundary: both CRs stay, and
		// "CR CR is identical to CR" for the handset.
		t.Fatalf("decoded %q", back)
	}
}

func TestUSSDTextUCS2(t *testing.T) {
	dcs, data, err := EncodeUSSDText("Bakiye: 5 ₺") // ₺ is not in the GSM alphabet
	if err != nil || dcs != DCSUCS2 {
		t.Fatalf("dcs %#x err %v", dcs, err)
	}
	if !bytes.Equal(data[:4], []byte{0x00, 'B', 0x00, 'a'}) || !bytes.Equal(data[len(data)-2:], []byte{0x20, 0xBA}) {
		t.Fatalf("UCS2 % X", data)
	}
	back, err := DecodeUSSDText(dcs, data)
	if err != nil || back != "Bakiye: 5 ₺" {
		t.Fatalf("%q %v", back, err)
	}
}

func TestUSSDTextLimits(t *testing.T) {
	if _, data, err := EncodeUSSDText(strings.Repeat("a", 182)); err != nil || len(data) != 160 {
		t.Errorf("182 GSM characters: %d octets, %v", len(data), err)
	}
	if _, _, err := EncodeUSSDText(strings.Repeat("a", 183)); err == nil {
		t.Error("183 GSM characters accepted")
	}
	if _, data, err := EncodeUSSDText(strings.Repeat("ж", 80)); err != nil || len(data) != 160 {
		t.Errorf("80 UCS2 characters: %d octets, %v", len(data), err)
	}
	if _, _, err := EncodeUSSDText(strings.Repeat("ж", 81)); err == nil {
		t.Error("81 UCS2 characters accepted")
	}
	if _, _, err := EncodeUSSDText(""); err == nil {
		t.Error("empty text accepted")
	}
}

// The CBS data coding scheme selects the alphabet (TS 23.038 §5).
func TestDecodeUSSDTextByDCS(t *testing.T) {
	gsm := unhex(t, "AA 98 6C 36 02")
	ucs := []byte{0x00, 'h', 0x00, 'i'}
	for _, c := range []struct {
		dcs  byte
		data []byte
		want string
	}{
		{0x0F, gsm, "*123#"}, // language unspecified
		{0x01, gsm, "*123#"}, // English, GSM 7-bit
		{0x48, ucs, "hi"},    // general data coding, UCS2
		{0x40, gsm, "*123#"}, // general data coding, GSM 7-bit
		{0x44, gsm, ""},      // 8-bit data
		{0x68, ucs, ""},      // compressed
		{0xF0, gsm, "*123#"}, // data coding group, GSM 7-bit
		{0xF4, gsm, ""},      // data coding group, 8-bit
		{0x80, gsm, "*123#"}, // reserved: read as GSM 7-bit
		{0x11, append(unhex(t, "65 37"), ucs...), "en" + "hi"}, // UCS2 after the language
	} {
		got, err := DecodeUSSDText(c.dcs, c.data)
		if err != nil || got != c.want {
			t.Errorf("dcs %#x: %q, %v; want %q", c.dcs, got, err, c.want)
		}
	}
	if _, err := DecodeUSSDText(0x48, []byte{0x00}); err == nil {
		t.Error("odd UCS2 accepted")
	}
}
