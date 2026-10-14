# Architecture overview

How the gateway is put together and how a message travels through it. For what is and is not
implemented, see the [README](../README.md#known-limitations).

## Layers

```
            NATS (JSON, ss7-3eai-gateway-client contract)
                │                      ▲
   cmd/gateway  │  nats_bridge.go      │  response / failure / MO publish
                ▼                      │
 internal/mapprotocol   MAP codec + operation handlers (TS 29.002)
                │                      ▲
 internal/tcap          dialogues, transaction IDs, timers, AC dispatcher (Q.771–Q.774)
                │                      ▲
 internal/sccp          UDT/UDTS codec, addressing, GTT, SLS (Q.711–Q.714)
                │                      ▲
 internal/m3ua          ASP over SCTP: association supervisor, DATA with routing label (RFC 4666)
                ▼                      │
            SG / STP  ──►  HLR, MSC, …
```

The layers are connected by bounded Go channels and wired in `cmd/gateway/main.go`. They start
bottom-up (M3UA, SCCP, MAP, CAP, TCAP) and stop in reverse order. The protocol libraries
underneath are [go-m3ua](https://github.com/wmnsk/go-m3ua) and
[go-tcap](https://github.com/wmnsk/go-tcap) (MIT); SCCP and the MAP codec are implemented here.

## An MT short message, end to end

1. **Request.** The SMSC calls `SendRoutingInfoForSM` through the client, which publishes on
   `map.sri.request` with a correlation ID. All gateway nodes subscribe in one NATS queue group,
   so exactly one node takes the request.
2. **Origination.** The NATS bridge validates the request, encodes the MAP argument and asks TCAP
   to originate a dialogue under `shortMsgGatewayContext-v3`. TCAP allocates a transaction ID
   (upper 8 bits = node number, lower 24 a counter) and remembers the correlation ID and the
   operation against it.
3. **Addressing.** The called address is the subscriber's MSISDN as a global title with SSN 6
   (the HLR), routed on GT: the STP's translation finds the subscriber's HLR, number portability
   included, as for any SMS-GMSC. (`map.sri_routing: hlr` sends it to one configured HLR instead.)
   The calling address carries this node's own GT and routes on GT, so the answer comes back to
   this node.
4. **Routing.** SCCP picks the DPC.
   - Route on DPC/SSN: the DPC is the called address's point code.
   - Route on GT: the highest-priority `sccp.gtt_rules` entry matching the called GT gives it.
   - An intermediate rule (`ssn` 0) sends the message to the STP still routing on GT; a final
     rule rewrites the address to route on point code and SSN.
   - The message is encoded as a UDT with the return option set, and the SLS rotates across links.
   - A message too long for one UDT (which must fit an MTP3 signalling information field) opens
     the dialogue with an "empty Begin" — the dialogue portion alone — and its components follow
     in the first CONTINUE once the peer has accepted (TS 29.002 §C.2.1). The gateway accepts
     that sequence from peers too.
5. **Transport.** M3UA writes a DATA message whose routing label carries OPC, DPC, SI = 3, the
   configured network indicator, and the SLS. The SCTP stream is chosen from the SLS.
6. **Answer.** The HLR's TC-END arrives on the association. SCCP checks that it is addressed to
   this node (its point code or one of its GTs) and passes it to TCAP. The dispatcher hands it to
   the MAP handler registered for the application context. The MAP handler decodes the
   ReturnResult against the stored operation, and the bridge publishes it on
   `map.sri.response` with the original correlation ID.
7. **MT-ForwardSM.** The SMSC then sends `MTForwardSM` with the IMSI and the serving MSC from the
   SRI answer. It goes out the same way under `shortMsgMT-RelayContext-v3`, addressed to the MSC's
   GT, which normally needs a GT rule toward the STP.

Every originated dialogue ends with exactly one publish on its response subject:

| Outcome | `status` | `error_code` |
|---|---|---|
| ReturnResult | `DELIVERED` | 0 |
| ReturnError | `REMOTE_ERROR` | the MAP error code (TS 29.002 MAP-ErrorCodes) |
| Reject, or ReturnError without a code | `REMOTE_ERROR` | -3 |
| No answer within `tcap.dialogue_timeout` | `TIMEOUT` | -1 |
| TC-ABORT from the peer | `ABORTED` | -2 |
| Not delivered: no SCCP route, M3UA could not send it, or the network returned it in a UDTS | `ABORTED` | -2 (the reason is in `error_message`) |
| A ReturnResult the gateway cannot decode | `REMOTE_ERROR` | -3 |

A request refused before a dialogue starts — a missing or invalid field, an address the gateway
cannot route, no capacity for another dialogue — is answered at once with `REJECTED` / -4 and the
reason. Only a request that is not valid JSON, or carries no correlation or message ID, goes
unanswered; the client validates its fields before publishing and bounds every call with a
deadline. An Invoke of an operation the gateway does not serve, under a context it does, is
answered with a TCAP Reject (unrecognizedOperation).

## Version negotiation

The gateway proposes version 3 of each short-message context. A peer that supports only version 2
refuses it with a TC-U-ABORT whose AARE proposes version 2; the gateway then sends the same
request in a new dialogue under version 2 (TS 29.002 §5.2.1) — MT delivery becomes forwardSM
there — once, and the caller sees only the final outcome. Answers to such a dialogue are routed
by a registration that applies only to dialogues the gateway opened, so peers cannot open
version 2 dialogues with it.

## Location and state

AnyTimeInterrogation and ProvideSubscriberInfo (TS 29.002 §8.11) are originated only.

- **AnyTimeInterrogation** (`map.ati.request`). The gateway acts as gsmSCF and asks the
  subscriber's HLR under `anyTimeInfoEnquiryContext-v3`. The called address is found as for
  SendRoutingInfoForSM: the MSISDN as global title with SSN 6, or the HLR the request or
  `map.sri_routing: hlr` names; a request by IMSI must name the HLR. The calling SSN is 147,
  gsmSCF (TS 23.003 §8.2), so it must be in `sccp.local_subsystems`. The argument carries a
  gsmSCF-Address the HLR may screen — configuration, never the request's, so a NATS publisher cannot
  pass as another gsmSCF: `map.gsmscf_address`, else the cluster
  GT, else the node's GT. An HLR that does not admit it answers `ati-NotAllowed` (49).
- **ProvideSubscriberInfo** (`map.psi.request`). The gateway takes the HLR's role and asks the VLR
  the request names, called SSN 7, calling SSN 6, under `subscriberInfoEnquiryContext-v3`. The
  IMSI and VLR number typically come from SendRoutingInfoForSM.

Both contexts exist in version 3 only, so there is no version fallback. The answer's
SubscriberInfo is decoded into `subscriber_info`: location (cell global identity, service area or
location area; VLR, MSC or SGSN number; age; whether it was obtained by paging), CS, PS and EPS
subscriber state, EPS location, IMEI, MS classmark 2 and number portability. Octet strings whose
layout another specification defines are passed through as hex.

## An MO short message

An MSC opens a dialogue under `shortMsgMO-RelayContext-v3`, addressed to the cluster GT. The STP
loadshares it to one node. That node decodes MO-ForwardSM, publishes it on `map.mo.sms`, and
answers the MSC with an empty result, its dialogue portion an AARE accepting the context: the
SMSC takes over delivery. A dialogue under a context the gateway does not implement is refused
with a TC-U-ABORT whose AARE names the version it does support, so the peer can retry with it. MO delivery over NATS is a
plain publish. Subscribers should use a queue group, and a message published while no subscriber
is connected is lost.

## SCCP role

The gateway is an SCCP end node, not a relay.
- An incoming message not addressed to it is refused with a UDTS, when the sender asked for one,
  and is never forwarded.
- An incoming UDTS returns one of our messages: the dialogue that message belonged to is ended at
  once and its caller told the return cause. It is never passed up as data.
- Global title translation applies only to messages this node originates.
- A peer's segmented XUDT is reassembled before delivery (Q.714 §4.1.1.2): segments are matched by
  calling address, OPC and local reference, wait at most T(reass) = 15 s, and a failure is
  returned in an XUDTS when the sender asked for it.

## High availability

Active-active, with no leader and no replicated dialogue state:

- **Inbound new dialogues.** All nodes join one M3UA application server in loadshare mode and
  share `sccp.cluster_global_title`. The SG spreads new dialogues addressed to that GT across
  the nodes. The node that receives the BEGIN owns the dialogue to its end.
- **Answers to dialogues a node started.** Each node uses its own `sccp.local_global_title` as
  calling address, so the network routes answers back to it. Transaction IDs also carry the node
  number.
- **Requests from applications.** They are spread by the NATS queue group. If a node dies, its
  in-flight dialogues get no answer at all: the caller's deadline expires, and its retry lands on
  a surviving node.
- **Shared configuration.** NATS JetStream KV holds configuration that the REST API manages.

## Configuration and operations

- **Configuration.** Read once at start-up from a YAML file. Secrets and per-node identity can come
  from `SS7_*` environment variables.
- **Probes.** `/api/v1/ready` is ready only while the M3UA association is up and the node is not
  draining. `/api/v1/health` is liveness.
- **Day-2 actions.** They are tokens in a watched file (`internal/opstrigger`):
  - `resetPeer` drops the association, and the supervisor re-establishes it.
  - `drain` fails readiness and stops taking new requests from NATS.
  - `reloadConfig` re-validates the configuration file.
  - Each outcome is reported as a log line and, in Kubernetes, as an Event.

## Testing approach

The end-to-end tests boot real gateway nodes through the production code path. Each test uses an
embedded NATS server and an in-process SS7 peer over loopback SCTP. The peer is built on the same
codecs as the gateway. It therefore does not trust them: it checks each frame against the
standards' expectations — routing label, SCCP routing indicator, application context per
operation — and records every violation, and any violation fails the test. Unit tests cover the
codecs, routing decisions, dialogue handling and the bridge in isolation.
