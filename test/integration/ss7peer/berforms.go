// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package ss7peer

import (
	"encoding/asn1"
	"fmt"
)

// indefiniteLengths rewrites b, a sequence of DER elements, with every
// constructed element in the indefinite length form and its end-of-contents
// (X.690 §8.1.3.6, Q.773 §4.1.2.3), as a peer whose encoder streams would
// send it. It is written here, apart from the gateway's code, so that the
// two do not share a reading of BER.
func indefiniteLengths(b []byte) ([]byte, error) {
	var out []byte
	for len(b) > 0 {
		var rv asn1.RawValue
		rest, err := asn1.Unmarshal(b, &rv)
		if err != nil {
			return nil, fmt.Errorf("indefiniteLengths: %w", err)
		}
		header := rv.FullBytes[:len(rv.FullBytes)-len(rv.Bytes)]
		if !rv.IsCompound {
			out = append(out, rv.FullBytes...)
		} else {
			content, err := indefiniteLengths(rv.Bytes)
			if err != nil {
				return nil, err
			}
			id := header[:1]
			if header[0]&0x1f == 0x1f { // high tag number: identifier runs to the octet with bit 8 clear
				n := 1
				for header[n]&0x80 != 0 {
					n++
				}
				id = header[:n+1]
			}
			out = append(out, id...)
			out = append(out, 0x80)
			out = append(out, content...)
			out = append(out, 0x00, 0x00)
		}
		b = rest
	}
	return out, nil
}
