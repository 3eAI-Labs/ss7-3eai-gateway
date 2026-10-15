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
bottom-up (M3UA, SCCP, MAP, CAP, TCAP) and stop in reverse order. M3UA (RFC 4666), SCCP, TCAP
(ITU-T Q.773) and the MAP codec are implemented here, each checked against frames built by hand
from its standard; SCTP is the kernel's, through
[ishidawataru/sctp](https://github.com/ishidawataru/sctp) (Apache-2.0).

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

## Call routing

SendRoutingInfo (TS 29.002 §10.1, `map.routing.request`) is originated only. The gateway acts as
a GMSC and asks the subscriber's HLR under `locationInfoRetrievalContext-v3`, addressed as for
SendRoutingInfoForSM, with calling SSN 8 (MSC, TS 23.003 §8.2). It sends a basic call
interrogation whose GMSC address is the cluster GT, else the node's GT — never the request's. It
offers no CAMEL, OR, CUG or pre-paging support, so an HLR answers as to a plain GMSC.

The answer is decoded into `routing_info` — the roaming number, or the forwarding data with its
decoded options, or CAMEL routing information passed through — with the IMSI in `imsi` and any
subscriber information in `subscriber_info`.

An HLR may split the answer: Return Result Not Last components, each in a TC-CONTINUE, then a
Return Result Last in the TC-END (TS 29.002 §C.2.3). The MAP layer holds the segments on the
dialogue, at most 16 of them and 16 KiB, and confirms once with them merged: a field comes from
the first segment that has it, and the ss-Lists are joined.

## USSD

USSD (TS 29.002 §11.9–11.11) runs under `networkUnstructuredSsContext-v2`, with the gateway as
gsmSCF (SSN 147). Unlike the other operations it is a session: one TCAP dialogue stays open while a
human reads and answers.

- **Subscriber-initiated.** The HLR opens a dialogue with processUnstructuredSS-Request and a
  MAP-OpenInfo naming the subscriber (IMSI or MSISDN, TS 29.002 table 7.3/2). The gateway decodes
  the dialled string and publishes a begin on `map.ussd.event.<route>`, the route chosen by the
  longest `map.ussd_routes` prefix (else `default`). The application answers with requests and
  notifies, and ends the session with the final text — processUnstructuredSS-Request's result, in
  the END — or a MAP error.
- **Network-initiated.** `map.ussd.start` (queue group) sends unstructuredSS-Request or -Notify to
  the subscriber's HLR, addressed as for SendRoutingInfoForSM, with a MAP-OpenInfo whose
  destinationReference is the MSISDN or the IMSI (numbering plan E.212) and whose
  originationReference is the gsmSCF address (`map.gsmscf_address`, else the cluster GT, else
  the node's GT). The application releases it with a basic END.

A session lives on the node that holds its dialogue. Commands (`map.ussd.command`) go to every
node; the session ID names the node, and only that one answers. The command subscription stays
up during a drain so open sessions can finish. A session waits for one thing at a time — the
answer to the request or notify outstanding, or the application's next command — for at most
`map.ussd_session_timeout` (timer ml, 1–10 minutes); its TCAP dialogue is given that plus 30 s, so
the session's own timer decides and ends it with a U-ABORT and an event. Results, errors and
endings are matched to the invoke outstanding; others are dropped. Every session ends exactly
once, with one end event: released (by the application or the network), the network's error,
aborted, timed out, a TCAP failure, or the node shutting down (sessions are aborted before the
stack stops).

## Location management, as HLR

The gateway is the MAP side of an HLR whose location management (TS 23.012) is an application:
the gateway speaks MAP to the VLRs, the application holds the subscribers and decides. SSN 6
must be in `sccp.local_subsystems`.

- **Location updates.** A VLR opens `networkLocUpContext-v3` with updateLocation, addressed to the
  E.214 global title of the IMSI (TS 23.003 §3.2). A `sccp.gtt_rules` entry naming
  `sccp.local_point_code` (with SSN 6) makes that range the gateway's own (see SCCP role). The
  MAP process in the HLR relays between the VLR and the application, which decides at each step
  (TS 29.002 figure 19.1.1/13). The gateway asks it on `map.ul`, as a NATS request, one turn at
  a time:
  - after the VLR's updateLocation (turn `update_location`);
  - after each insertSubscriberData the gateway sent (turn `insert_result`).
  The NATS reply comes back to the node holding the dialogue. Each answer is one of: insert
  subscriber data, accept (updateLocation's result, with the HLR number: `map.hlr_number`, else
  the cluster GT, else the node GT), or reject with one of updateLocation's errors.
- **Subscriber data** goes in insertSubscriberData within the dialogue, without the IMSI
  (§8.8.1.3). Data that does not fit one SCCP unitdata is split over several, one outstanding at
  a time: the subscriber's own data first (networkAccessMode may only be in the first), then the
  service lists — never divided, since some bearer groups go as a pair — and each supplementary
  service, packed in order. The parts' results go back as one.
- **Endings and timers.** The application has `map.hlr_answer_timeout` (1–10 s, default 5 s) per
  turn. No answer in time, no application listening (NATS reports no responder at once), or an
  answer the gateway cannot carry out refuses the update with systemFailure, and the application
  gets an `end` turn saying so. Data the gateway cannot send, and the VLR's error to it, go back
  to the application as an `insert_result` with the failure, for it to decide. The gateway waits for the VLR's answer to subscriber data for
  timer m's upper limit, 30 s. The VLR's silence, its abort or END, a TCAP timeout or the node
  shutting down end the update with an `end` turn, which is not answered. An update ends either
  by the application's accept or reject, carried out, or with exactly one `end` turn. The `end`
  waits for any turn in flight, so turns come one at a time and an `end` after an answer means
  the answer was not carried out.
- **Towards a VLR.** cancelLocation (`map.cl.request`), provideRoamingNumber (`map.prn.request`)
  and a stand-alone insertSubscriberData (`map.isd.request`, which carries the IMSI) go to the
  VLR the request names: its number as GT with SSN 7, or a point code. They are sent under
  `locationCancellationContext-v3`, `roamingNumberEnquiryContext-v3` and
  `subscriberDataMngtContext-v3`, answered to SSN 6, every outcome on the response subject.
  A stand-alone insert must fit one message.

All four contexts are used in version 3 only. A VLR that opens `networkLocUpContext-v2` is
refused with an AARE proposing version 3.

## Message order

SCCP and TCAP each run a pool of workers. Neither shares one queue among them:
- SCCP gives an incoming message to a worker chosen by its OPC and SLS. MTP delivers one OPC and
  SLS in sequence, which is what protocol class 1 relies on (Q.714 §1.1.2.2).
- TCAP chooses by transaction: the destination transaction ID, or for a BEGIN the origination ID.

The messages of one transaction are therefore handled in the order they arrived. A segment in a
TC-CONTINUE is not overtaken by the TC-END that follows it, as long as MTP delivers them in order.
That holds when the peer sends them with one SLS (protocol class 1, which TS 29.002 §C.2.3
recommends for TC-Result-NL), but not when it rotates the SLS. A SendRoutingInfo result whose
IMSI was lost that way is reported as incomplete (`REMOTE_ERROR`), not passed on as an answer.

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
- Routing follows Q.714 §2.2.1. A message routed on SSN is for this node, whatever point code its
  called address carries. One routed on GT is for this node when the GT is one of its own, or
  when translation here gives the node's own point code; the rule's SSN, if any, replaces the
  called SSN. That is how an HLR's E.214 range is taken: `sccp.gtt_rules` with
  `point_code: <sccp.local_point_code>, ssn: 6, numbering_plan: 7`. A rule naming the local point
  code must name a numbering plan, since an E.214 range begins with the digits of the network's
  own E.164 numbers (TS 23.003 §3.2); without it SRI-for-SM to an MSISDN of the range would be
  delivered here. A message routed on SSN is ours when MTP delivered it to our point code. Any
  other GT is refused, not relayed.
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
- **Dialogues a peer opened.** The node's answers carry its own GT as calling address too, not
  the cluster GT or E.214 GT the BEGIN was sent to (TS 29.002 §7.3.1, responding address). The
  peer sends the rest of the dialogue there, so a dialogue of several exchanges stays on its
  node: a USSD session, or a location update with insertSubscriberData.
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
