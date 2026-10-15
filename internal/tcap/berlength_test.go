// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package tcap

import (
	"bytes"
	"encoding/asn1"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// The BER length forms of X.690 §8.1.3 a peer may send: definite short
// (§8.1.3.4), definite long in any number of octets (§8.1.3.5) and, for a
// constructed element, indefinite with end-of-contents (§8.1.3.6).

func TestDefiniteLengths(t *testing.T) {
	long := strings.Repeat(" AA", 128)
	for name, c := range map[string]struct{ in, want string }{
		"definite short":                     {"30 03 02 01 05", "30 03 02 01 05"},
		"definite long, minimal":             {"04 81 80" + long, "04 81 80" + long},
		"indefinite":                         {"30 80 02 01 05 00 00", "30 03 02 01 05"},
		"indefinite in indefinite":           {"30 80 30 80 02 01 05 00 00 00 00", "30 05 30 03 02 01 05"},
		"indefinite in definite":             {"30 07 30 80 02 01 05 00 00", "30 05 30 03 02 01 05"},
		"indefinite, empty":                  {"30 80 00 00", "30 00"},
		"long form below 128":                {"30 81 03 02 01 05", "30 03 02 01 05"},
		"long form with a leading zero":      {"04 82 00 03 01 02 03", "04 03 01 02 03"},
		"long form of 128, leading zeros":    {"04 84 00 00 00 80" + long, "04 81 80" + long},
		"long form in indefinite":            {"30 80 04 81 01 07 00 00", "30 03 04 01 07"},
		"high tag number, indefinite":        {"BF 1F 80 02 01 05 00 00", "BF 1F 03 02 01 05"},
		"two elements, the second rewritten": {"05 00 30 80 05 00 00 00", "05 00 30 02 05 00"},
	} {
		got, err := definiteLengths(frame(t, c.in))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if want := frame(t, c.want); !bytes.Equal(got, want) {
			t.Errorf("%s:\n got % X\nwant % X", name, got, want)
		}
	}
}

// A definite element whose content is not BER elements goes on as it came,
// to be answered by its decoder (Q.774: a Reject of the component), even
// where an indefinite length around it is rewritten.
func TestDefiniteLengthsOpaqueContent(t *testing.T) {
	for in, want := range map[string]string{
		"30 03 01 02 03":             "30 03 01 02 03",
		"30 81 03 04 05 01":          "30 03 04 05 01",
		"30 80 30 03 01 02 03 00 00": "30 05 30 03 01 02 03",
		"30 04 30 80 05 00":          "30 04 30 80 05 00",
	} {
		got, err := definiteLengths(frame(t, in))
		if err != nil || !bytes.Equal(got, frame(t, want)) {
			t.Errorf("% s: got % X (%v), want %s", in, got, err, want)
		}
	}
}

// What is already definite and minimal comes back as it is, not copied.
func TestDefiniteLengthsKeepsCanonical(t *testing.T) {
	in := frame(t, berFrames["BEGIN, AARQ, Invoke"].hex)
	got, err := definiteLengths(in)
	if err != nil || &got[0] != &in[0] {
		t.Fatalf("got a copy (%v)", err)
	}
}

func TestDefiniteLengthsRejects(t *testing.T) {
	for name, h := range map[string]string{
		"indefinite primitive":         "04 80 01 00 00",
		"no end-of-contents":           "30 80 02 01 05",
		"reserved length octet":        "30 FF",
		"length octets truncated":      "04 82 00",
		"content beyond the message":   "04 05 01 02",
		"indefinite, child beyond it":  "30 80 04 05 01 00 00",
		"identifier truncated":         "1F 81",
		"length missing":               "30",
		"long form beyond the message": "04 88 FF FF FF FF FF FF FF FF",
		"nested too deep":              strings.Repeat("30 80 ", maxBERNesting+2) + strings.Repeat("00 00 ", maxBERNesting+2),
	} {
		if got, err := definiteLengths(frame(t, h)); err == nil {
			t.Errorf("%s: got % X, want an error", name, got)
		}
	}
}

// The same TCAP message, written by hand from Q.773 §3 with every
// constructed element of indefinite length (§4.1.2.3) — the parameter's
// SEQUENCE included — parses to what its definite form does.
func TestParseMessageIndefinite(t *testing.T) {
	m, err := ParseMessage(frame(t, "62 80"+
		" 48 04 00 00 00 05"+
		" 6B 80 28 80 "+dialogueAsIDHex+
		" A0 80 60 80"+
		" 80 02 07 80"+
		" A1 80 06 07 04 00 00 01 00 14 03 00 00"+
		" 00 00 00 00"+ // AARQ, single-ASN1-type
		" 00 00 00 00"+ // EXTERNAL, dialogue portion
		" 6C 80 A1 80"+
		" 02 01 01 02 01 2D"+
		" 30 80 80 01 00 00 00"+
		" 00 00 00 00"+ // invoke, component portion
		" 00 00"))
	if err != nil {
		t.Fatal(err)
	}
	if want := berFrames["BEGIN, AARQ, Invoke"].msg; !reflect.DeepEqual(m, want) {
		t.Fatalf("got %#v\nwant %#v", m, want)
	}
}

// The END with a negative invoke ID, every length in a longer form than it
// needs (X.690 §8.1.3.5 Note 2: the sender may choose).
func TestParseMessageNonMinimalLengths(t *testing.T) {
	m, err := ParseMessage(frame(t, "64 81 17"+
		" 49 82 00 04 00 00 00 05"+
		" 6C 81 0C A3 83 00 00 07 02 01 FF 02 81 01 22"))
	if err != nil {
		t.Fatal(err)
	}
	if *m.Components[0].InvokeID != -1 || *m.Components[0].ErrorCode != 34 || !bytes.Equal(m.DestinationTID, tid5) {
		t.Fatalf("%+v", m)
	}
}

// Every frame of ber_test.go, rewritten once with indefinite lengths and
// once with four-octet long forms, parses to the same message.
func TestParseMessageEveryLengthForm(t *testing.T) {
	for name, c := range berFrames {
		def := frame(t, c.hex)
		for form, rewrite := range map[string]func(testing.TB, []byte) []byte{
			"indefinite": indefiniteForm, "long form": longForm,
		} {
			b := rewrite(t, def)
			if bytes.Equal(b, def) {
				t.Fatalf("%s, %s: rewrite changed nothing", name, form)
			}
			got, err := ParseMessage(b)
			if err != nil {
				t.Errorf("%s, %s: %v", name, form, err)
				continue
			}
			if !reflect.DeepEqual(got, c.msg) {
				t.Errorf("%s, %s:\n got %#v\nwant %#v", name, form, got, c.msg)
			}
		}
	}
}

// A transaction's messages go to one worker whatever lengths each carries.
func TestTransactionShardEveryLengthForm(t *testing.T) {
	for _, tid := range []string{"00 00 00 05", "00 00 01 00", "12 34 56 78", "FF"} {
		n := len(strings.Fields(tid))
		def := frame(t, fmt.Sprintf("65 %02X 48 %02X %s 49 %02X %s", 2*(2+n), n, tid, n, tid))
		want := transactionShard(def, 16)
		for form, b := range map[string][]byte{"indefinite": indefiniteForm(t, def), "long form": longForm(t, def)} {
			if got := transactionShard(b, 16); got != want {
				t.Errorf("TID %s, %s: worker %d, want %d", tid, form, got, want)
			}
		}
	}
}

// indefiniteForm rewrites every constructed element of b, a sequence of
// DER elements, into the indefinite length form.
func indefiniteForm(t testing.TB, b []byte) []byte {
	return rewriteLengths(t, b, func(compound bool, content []byte) []byte {
		if !compound {
			return appendLength(nil, len(content))
		}
		return []byte{0x80}
	}, true)
}

// longForm rewrites every length of b into the four-octet long form.
func longForm(t testing.TB, b []byte) []byte {
	return rewriteLengths(t, b, func(_ bool, content []byte) []byte {
		n := len(content)
		return []byte{0x84, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	}, false)
}

func rewriteLengths(t testing.TB, b []byte, length func(bool, []byte) []byte, eoc bool) []byte {
	t.Helper()
	var out []byte
	for len(b) > 0 {
		var rv asn1.RawValue
		rest, err := asn1.Unmarshal(b, &rv)
		if err != nil {
			t.Fatal(err)
		}
		id, err := asn1.Marshal(asn1.RawValue{Class: rv.Class, Tag: rv.Tag, IsCompound: rv.IsCompound})
		if err != nil {
			t.Fatal(err)
		}
		content := rv.Bytes
		if rv.IsCompound {
			content = rewriteLengths(t, rv.Bytes, length, eoc)
		}
		out = append(out, id[:len(id)-1]...) // identifier, without Marshal's zero length
		out = append(out, length(rv.IsCompound, content)...)
		out = append(out, content...)
		if eoc && rv.IsCompound {
			out = append(out, 0, 0)
		}
		b = rest
	}
	return out
}
