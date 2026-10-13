# Integration tests

## End-to-end SS7 round trip (CI gate)

The end-to-end tests in `cmd/gateway/*_test.go` boot the gateway through the same code path as
`main()` and drive the full stack against an in-process SS7 peer over a real loopback SCTP
association:

```
NATS request → MAP encode → TCAP BEGIN → SCCP UDT → M3UA → SCTP → ss7peer (HLR / MSC)
            ← NATS response ← MAP decode ← TCAP END + ReturnResult ← SCCP UDT ← M3UA ←
```

| Test | Covers |
|------|--------|
| `TestE2E_SRIForSM_RoundTrip` | SRI-for-SM: IMSI and network node number back on `map.sri.response`; calling-party GT |
| `TestE2E_MtForwardSM_RoundTrip` | MT-Forward-SM: called-party GT from the network node number, TPDU pass-through, `DELIVERED` |
| `TestE2E_Client_SRIThenMT` | The same two operations through the Go client module (`ss7-3eai-gateway-client`) |
| `TestE2E_Cluster_3Node_NATSLoadShare` | Three nodes share the NATS queue group; every node takes traffic and each response returns to the node that originated the dialogue |
| `TestE2E_Client_RemoteErrors` | A MAP ReturnError reaches the caller as `REMOTE_ERROR` with the MAP error code |
| `TestE2E_Cluster_NodeOutage_FailureSinkTimeout` | Dialogues whose answers are dropped come back as `TIMEOUT`, one response per request; after a node stops, the remaining two serve all traffic |
| `TestM3UA_StartWithoutSGWThenSGWAppears` (`internal/m3ua`) | Start-up without a reachable SG does not fail; the association comes up when the SG does |
| `TestM3UA_ReconnectsAfterAssociationLoss` (`internal/m3ua`) | The association is re-established after each of three losses and stays up |

The peer does not trust the gateway's encodings, although it shares its codecs: it checks the MTP3
routing label (OPC, DPC, SI, NI), the SCCP routing indicator of both addresses and the MAP
application context of each operation, answers a wrong context with a TC-U-ABORT, and records
every violation. Each test fails on any recorded violation (`peer.Violations()`).

They need Linux with kernel SCTP and are kept out of `go test ./...` by the `integration` build tag:

```bash
sudo apt-get install -y libsctp-dev lksctp-tools && sudo modprobe sctp
go test -tags integration -count=1 -timeout 180s ./cmd/gateway/ ./internal/m3ua/
# or: make test-e2e
```

`ss7peer/` is the programmable peer: an M3UA listener with the gateway's own SCCP codec, TCAP via
`wmnsk/go-tcap`, and MAP callbacks (`OnSRI`, `OnMtForwardSM`) that tests use to assert on what the
gateway sent and to choose the answer.

## Manual helpers

This predates the end-to-end suite, talks to NATS only, and is not run in CI.

- `nats_load_simulator.go` — publishes and/or subscribes JSON messages on the NATS subjects at a
  fixed rate. It does not speak SS7: it measures the NATS side only.
  `go run nats_load_simulator.go -mode both -rate 100 -duration 60`
