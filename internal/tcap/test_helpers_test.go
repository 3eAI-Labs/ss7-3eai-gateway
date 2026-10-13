// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package tcap

import (
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
)

// stubChannels returns a pair of buffered SCCP channels suitable for wiring a
// Handler in tests. The channels are buffered (size 16) so tests that exercise
// the outgoing path do not block on a missing drainer; tests that explicitly
// observe sccpOut should keep an extra reference and drain.
func stubChannels() (chan *sccp.SccpMessage, chan *sccp.SccpMessage) {
	in := make(chan *sccp.SccpMessage, 16)
	out := make(chan *sccp.SccpMessage, 16)
	return in, out
}
