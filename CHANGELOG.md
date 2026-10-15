# Changelog

All notable changes to `ss7-3eai-gateway` are documented in this file.
Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versioning follows [SemVer](https://semver.org/).

## [Unreleased]

## [0.7.9] - 2026-10-15

### Added

- **Authentication and MS purging, as HLR** (TS 29.002 §8.5.2, §8.1.6), with client module
  v0.6.0. The application is the AuC and the HLR's subscriber records; the gateway is their MAP
  side.
  - **SendAuthenticationInfo** (op 56) under `infoRetrievalContext-v3`, from a VLR or SGSN. The
    node holding the dialogue asks the application on `map.sai`, as a NATS request, with the
    number of vectors wanted, the requesting node type and PLMN, and the re-synchronisation data
    (RAND, AUTS) when there is any. The application answers with GSM triplets, UMTS quintuplets
    or EPS vectors (and the UE usage type when asked), or one of the operation's errors; no
    vectors is the empty result. No answer within `map.hlr_answer_timeout`, no application
    (refused at once) or an answer the gateway cannot carry out: systemFailure.
  - **Vectors in parts.** Vectors that do not fit one message go in several results, as TS 29.002
    Annex C.3.3 recommends for this operation: each a ReturnResultLast, all but the last in a
    CONTINUE, the VLR asking for the next within timer m (30 s); a Reject of a part closes the
    dialogue. Five UMTS quintuplets take three messages. A request with segmentationProhibited
    gets what fits one END.
  - Vector counts per kind (§8.5.2.1): an MME gets EPS vectors only; a combined MME/SGSN its
    requested number of the kind for immediate use and its additional number of the other; any
    other node triplets or quintuplets only. A missing requesting node type is passed on as
    absent, not as "vlr".
  - **PurgeMS** (op 67) under `msPurgingContext-v3`. The application, asked on `map.purge`, marks
    the subscriber purged and says which of TMSI, P-TMSI and M-TMSI to freeze, or refuses with
    one of the operation's errors. purgeMS has no systemFailure: without a usable answer in time
    the dialogue is aborted.
  - Vectors are never logged. NATS permissions for `map.sai` and `map.purge` are described in the
    README (Access).
  - Codecs checked against frames built by hand from the ASN.1, mutation-checked; the decoders of
    peer input are fuzzed. The strict peer plays the VLR, checking the AARE, that every part is a
    ReturnResultLast of the request it answers, no more vectors than asked for, and the
    operations' errors. A three-node cluster test shows each request in parts stays on its node.
- Chart: capability `ss7-map-authentication` (responds `map.sai`); `ss7-map-mobility` also
  responds `map.purge`.

### Changed

- A Reject of a second Invoke in the BEGIN of a dialogue the gateway answers is placed by one
  rule for location updates and the new requests: in a CONTINUE when the dialogue has a request
  alive, in an END otherwise. In the new dialogues, an Invoke in the peer's END and components
  after the gateway's own END are dropped, not answered.

## [0.7.8] - 2026-10-15

### Added

- **Location management, as HLR** (TS 29.002 §8.1, §8.8.1, §10.2), with client module v0.5.0.
  The application is the HLR's location management (TS 23.012); the gateway is its MAP side.
  - **Location updates.** A VLR's updateLocation (op 2) under `networkLocUpContext-v3` is
    accepted. The node holding the dialogue asks the application on `map.ul`, as a NATS request,
    at each decision of TS 29.002 figure 19.1.1/13: after the VLR's request, and after each
    insertSubscriberData (op 7) the gateway sent in the dialogue.
    - The application answers: insert subscriber data, accept (updateLocation's result, with
      `map.hlr_number`, else the cluster GT, else the node GT), or reject with one of
      updateLocation's errors.
    - Subscriber data that does not fit one message is sent in several insertSubscriberData,
      one at a time: the subscriber's own data first, no service list divided. Their results go
      back as one.
    - The application has `map.hlr_answer_timeout` per decision (1–10 s, default 5 s). No answer,
      no application (refused at once) or an answer the gateway cannot carry out: systemFailure.
    - The VLR's error to the data, or data the gateway cannot send, goes back to the application
      to decide. The VLR's silence (timer m, 30 s), abort or END, a TCAP timeout and node shutdown
      end the update with an `end` turn.
  - **Subscriber data** (circuit-switched): MSISDN, category, status, bearer and teleservices,
    call forwarding, call barring, other SS data, operator determined barring, regional
    subscription, network access mode, access restriction. Written in declaration order, which
    for the extension fields is not tag order.
  - **Towards a VLR:** cancelLocation (op 3) on `map.cl.request`, provideRoamingNumber (op 4) on
    `map.prn.request`, and a stand-alone insertSubscriberData on `map.isd.request`. Each is
    addressed to the VLR's number (SSN 7) or a point code, answered to SSN 6, under
    `locationCancellationContext-v3`, `roamingNumberEnquiryContext-v3` and
    `subscriberDataMngtContext-v3`.
  - Codecs checked against frames built by hand from the ASN.1; every decoder of peer input is
    fuzzed. The strict peer plays the VLR. A three-node cluster test shows each location update
    finishing on the node that took its BEGIN.
  - MAP error noRoamingNumberAvailable (39).
- **SCCP routing per Q.714 §2.2.1.** A message routed on GT whose translation here gives this
  node's point code is accepted, with the rule's SSN. A `sccp.gtt_rules` entry naming
  `sccp.local_point_code` thus makes a GT range the gateway's own, as an HLR's E.214 range. A
  message this node sends to such a GT is delivered locally.
- `sccp.gtt_rules[].numbering_plan` limits a rule to one numbering plan. A rule naming the local
  point code must set it — an E.214 range begins with the digits of the network's own E.164
  numbers, so without it SRI-for-SM to an MSISDN of the range would be delivered to the gateway
  itself. The gateway refuses such a rule at start-up.
- Helm chart: `map.hlr_number`, `map.hlr_answer_timeout`; capability `ss7-map-mobility`.

### Fixed

- **Cluster:** the answers to a dialogue a peer opened carried the called address of its BEGIN —
  the cluster GT — as calling address. The peer then sent the rest of the dialogue to the
  cluster GT, which the SG may load-share to another node, one that does not hold the dialogue.
  The three-node location update test fails that way without the fix. USSD sessions a subscriber
  starts (0.7.7) take the same code path, so in a cluster they could lose their continuation once
  they ran past one exchange; that is read from the code, not run in a cluster test. With
  `sccp.local_global_title` set, these answers now carry the node's own GT (TS 29.002 §7.3.1:
  responding address).
- SCCP: a message routed on SSN that MTP delivered to our point code is accepted whatever point
  code its called address carries, instead of being refused (Q.714 §2.2.1: the receiving node is
  the destination).
- MAP: a BEGIN with a USSD request (or now updateLocation) and a second Invoke rejected the second
  in an END, ending the dialogue under the session it had just opened. The Reject now goes in a
  CONTINUE when a session lives, and nothing in a message runs after a Reject that ended the
  dialogue.
- SCCP: numbering plan 7 is ISDN/mobile (E.214), not "private" (Q.713 §3.4.2.3.3).

## [0.7.7] - 2026-10-15

### Added

- **USSD** (TS 29.002 §11.9–11.11), with client module v0.4.0, under
  `networkUnstructuredSsContext-v2` with the gateway as gsmSCF (SSN 147).
  - Subscriber-initiated: the HLR's processUnstructuredSS-Request (op 59) is published as a begin
    on `map.ussd.event.<route>`. The route comes from the longest `map.ussd_routes` prefix of the
    dialled string, else `default`. The subscriber is identified from the MAP-OpenInfo (IMSI or
    MSISDN) and the argument's MSISDN. The application ends the session with the final text or a
    MAP error.
  - Network-initiated: `map.ussd.start` sends unstructuredSS-Request (60) or -Notify (61) to the
    subscriber's HLR by MSISDN or IMSI, with a MAP-OpenInfo naming the subscriber and the gsmSCF.
  - Commands on `map.ussd.command` (request, notify, release, reject) are answered on
    `map.ussd.response` by the node holding the session.
  - Every session ends once, with one end event: released, the network's error, aborted,
    `map.ussd_session_timeout` (1–10 min, default 180 s), TCAP failure or node shutdown.
  - USSD text: GSM 7-bit with the extension table, USSD packing and CR padding, UCS2, and the
    CBS data coding schemes (TS 23.038).
  - MAP-OpenInfo codec (TS 29.002 §17.4); MAP errors unknownAlphabet (71) and ussd-Busy (72).
  - The strict peer plays the HLR in both directions and checks the context, subsystems,
    MAP-OpenInfo, transaction IDs, AARE and invoke IDs.
- TCAP: a peer's processUnstructuredSS-Request after an empty Begin (TS 29.002 §C.2.1) is
  accepted, its MAP-OpenInfo kept from the BEGIN.
- Helm chart: `map.ussd_session_timeout`, `map.ussd_routes`; capability `ss7-map-ussd`.

### Fixed

- TCAP: a dialogue the peer never answered times out without an attempt to send an ABORT that no
  transaction ID could address (it used to log an encode failure).
- TCAP: the peer's transaction ID, set by a CONTINUE, is read safely from other goroutines.

## [0.7.6] - 2026-10-14

### Added

- **Call routing** (TS 29.002 §10.1), with client module v0.3.0: SendRoutingInfo (op 22) on
  `map.routing.request` → `map.routing.response`.
  - The gateway acts as a GMSC and asks the subscriber's HLR under
    `locationInfoRetrievalContext-v3`. It addresses the HLR like SendRoutingInfoForSM, with calling
    SSN 8.
  - It sends a basic call interrogation. The GMSC address is the cluster GT, else the node's GT;
    a request cannot set it.
  - The answer is decoded into `routing_info`: the roaming number, or the forwarded-to number with
    decoded forwarding options, or CAMEL routing information passed through. It also carries the
    VMSC address, basic MSISDN, number portability status, basic service and SS codes. The IMSI
    goes in `imsi` and any subscriber information in `subscriber_info`.
  - The decoder is fuzzed, and both directions are checked against octets built by hand from the
    ASN.1. Received ISDN addresses are accepted up to 20 octets (an NPLR's routing number plus
    MSISDN can exceed the type's 9); the gateway sends at most 9.
  - Peers cannot open the context with the gateway.
- **Segmented results** (TS 29.002 §C.2.3). An HLR may send its answer as Return Result Not Last
  components in TC-CONTINUEs, followed by a Return Result Last in the TC-END.
  - TCAP now tells the two apart; it used to take the first segment as the whole result.
  - The segments are merged into one answer for SendRoutingInfo.
  - Beyond 16 segments or 16 KiB, the caller gets `REMOTE_ERROR`.
  - A SendRoutingInfo result without the IMSI is incomplete (TS 29.002: present in the result or
    in one of its segments) and is reported as `REMOTE_ERROR`. This also catches a segment
    overtaken by the last one when a peer rotates the SLS.
- MAP error names for numberChanged (44), busySubscriber (45), noSubscriberReply (46) and
  or-NotAllowed (48).
- Helm chart: capability `ss7-map-call-routing` in `arinna-capabilities.provides`.

### Fixed

- **M3UA message order.** The M3UA library parsed every received SCTP message, and handled every
  DATA, in a goroutine of its own. Messages of one signalling relation could therefore reach SCCP
  in any order, against RFC 4666 §1.4.7. A result in TC-Result-NL segments could fail on that
  (seen in CI), and so could anything relying on MTP's in-sequence delivery. M3UA is now
  implemented here (`internal/m3ua/wire`):
  - one reader per association, messages delivered in arrival order;
  - DATA never on stream 0;
  - BEAT answered;
  - NTFY, ERR and SSNM logged;
  - messages up to 64 KiB, where the library read 1500 octets at a time.

  The codec is checked against frames built from RFC 4666. The library is gone from the module,
  and the E2E tests run with the race detector again. That found, and this release fixes, a data
  race on a dialogue's expiry time.
- **SCTP Payload Protocol Identifier.**
  - The DATA chunks carried PPID 0x03000000 instead of M3UA's 3 (RFC 4666 §7.1: "any other values
    MUST not be used"). The value was converted to network order twice, once by the M3UA library and
    once by the SCTP library.
  - A signalling gateway, STP or firewall that checks the PPID could drop or misroute all traffic.
- **M3UA conformance.**
  - "Alternate ASP Active" now takes this ASP out of service and brings it back. It used to be
    logged while the link kept showing up.
  - ERR carries the offending message, and an ERR is never answered with an ERR.
  - An association with a single stream is refused.
  - ASP Up Ack and ASP Active Ack keep separate timeouts.
- **M3UA reconnect after EISCONN.** A connect restarted after a signal can report EISCONN for an
  association that came up meanwhile. The SCTP library then closes the association. The dial is
  now repeated at once instead of after the reconnect interval. This was seen once in CI; the
  cause is our reading of it, and I could not reproduce it locally.
- **Message order within a transaction.** SCCP and TCAP each handed incoming messages to a pool
  of workers through one shared queue, so two messages of one transaction could be processed in
  either order. A TC-CONTINUE carrying a result segment could be overtaken by the TC-END that
  completes it. SCCP now picks the worker by OPC and SLS, which MTP keeps in sequence (Q.714
  §1.1.2.2 — protocol class 1), and TCAP by transaction ID.

### Removed

- The unused inbound SendRoutingInfo handler skeleton (an HLR role the gateway does not have).
  Its result encoder lacked the `[3]` tag of SendRoutingInfoRes.

## [0.7.5] - 2026-10-14

### Fixed

- **TCAP encoding (ITU-T Q.773).** Releases 0.7.0–0.7.4 used a TCAP library whose encoding is not
  Q.773's, so their dialogues were very unlikely to work with real SS7 equipment:
  - **Parameter framing.** Every operation's argument was wrapped in an extra SEQUENCE (`30 L 30 L …`
    instead of `30 L …`). On receipt, a component's parameter lost its own tag and length — for
    SendRoutingInfoRes, the `[3]`. Q.773 carries the parameter as `ANY DEFINED BY operationCode`:
    the argument's own value, as is.
  - **Lengths of 128 octets or more** were written in one octet — not valid BER (Q.773 §4.1.1 asks
    for the minimal long form). Every MT-ForwardSM with a real short message is that long.
    Long-form lengths from peers were misread.

  TCAP is now encoded and decoded here, without the library. Frames built by hand from Q.773 check
  every message type, every component type, the dialogue PDUs (AARQ, AARE accepted or refusing a
  context, ABRT), signed invoke IDs and long-form lengths.

  The test peer used the same library, so both ends agreed and the end-to-end tests passed. The
  peer now uses this codec, which is itself checked against the Recommendation.
- **One outcome per request.**
  - An END that carried no result and no error — or only the segments of a result — left the
    caller waiting until its own deadline. It is now answered with `REMOTE_ERROR`, "dialogue
    ended without a result".
  - A result that arrived in a CONTINUE was followed, when the dialogue timer ran out, by a second
    answer, `TIMEOUT`. Now each originated dialogue gives its caller exactly one outcome, and
    anything after the first is dropped.
- A global title, MSISDN, network node number or VLR number taken from a request may have at most
  16 digits. A longer one could corrupt the SCCP called party address, whose length field is one
  octet; it is now `REJECTED`.

## [0.7.4] - 2026-10-14

### Added

- Location and state (TS 29.002 §8.11), with client module v0.2.0:
  - **AnyTimeInterrogation** (op 71) on `map.ati.request` → `map.ati.response`: the gateway asks
    the subscriber's HLR as gsmSCF under `anyTimeInfoEnquiryContext-v3` — addressed like
    SendRoutingInfoForSM, calling SSN 147 — with the gsmSCF-Address from `map.gsmscf_address`,
    else the cluster GT, else the node's GT. Requests cannot set it: the HLR admits ATI by that
    address, and a NATS publisher must not pass as another gsmSCF.
  - **ProvideSubscriberInfo** (op 70) on `map.psi.request` → `map.psi.response`: the gateway asks
    the serving VLR under `subscriberInfoEnquiryContext-v3`, called SSN 7, calling SSN 6.
  - The answer is decoded into `subscriber_info`: cell global identity, service area or location
    area, VLR/MSC/SGSN number, age of location, paging, CS/PS/EPS subscriber state, EPS location,
    IMEI, MS classmark 2, number portability. Decoders are fuzzed; the encoders are checked
    against octets built by hand from the ASN.1.
  - Peers cannot open either context with the gateway.
- `REJECTED` (-4): a request refused before a dialogue starts — invalid, unroutable, or no capacity
  for another dialogue — is answered at once on its response subject, for every request subject.
  It used to get no answer at all.

### Changed

- `sccp.local_subsystems` defaults to `[6, 8, 146, 147]` in the chart and the example
  configuration: AnyTimeInterrogation answers come back to SSN 147. A configuration without 147
  gets `REJECTED` for AnyTimeInterrogation. The example's comment called 146 "gsmSCF"; it is CAP,
  and gsmSCF (MAP) is 147 (TS 23.003 §8.2).
- An Invoke of an operation the gateway does not serve, under a context it does, is answered with
  a TCAP Reject (unrecognizedOperation) instead of no answer.
- A ReturnResult the gateway cannot decode ends the request with `REMOTE_ERROR` (-3) instead of
  leaving the caller to its deadline.
- Addresses that become a global title — an HLR `global_title` override, MT-ForwardSM's
  `network_node_number`, ProvideSubscriberInfo's `vlr_number` — must be decimal digits; anything
  else (`+90…`) is `REJECTED` instead of going out as a corrupted SCCP address.

### Fixed

- **Nature of address of MAP AddressStrings.** The MSISDN of SendRoutingInfoForSM, the MSISDN form
  of SM-RP-OA, and every address given as a bare digit string were encoded with nature of address
  4, which in a TS 29.002 AddressString means "subscriber number"; international is 1 (4 is
  international in an SCCP global title, Q.713, where the value had come from). They now go out as
  international (first octet 0x91). Applications that build an address object with `"nature": 4`
  meaning international must send 1; client v0.2.0's `E164()` does. The test peer shared the
  gateway's codec, so it could not see this; the encoders are now also checked against
  hand-built octets.

## [0.7.3] - 2026-10-14

### Added

- Version fallback (TS 29.002 §5.2.1). A peer that supports only version 2 of the short-message
  contexts refuses the gateway's version 3 dialogue with a TC-U-ABORT whose AARE proposes version
  2; the request failed with `ABORTED`. The gateway now starts a new dialogue under the proposed
  version 2 context — SRI-for-SM unchanged under shortMsgGateway v2, MT delivery as forwardSM
  (op 46) under shortMsgMT-Relay v2 — once per request; the caller sees only the final outcome.
  The version 2 forwardSM argument is sent as MT-ForwardSM's (sm-RP-DA, sm-RP-OA, sm-RP-UI); that
  this matches MAP v2's ForwardSM-Arg is from knowledge of MAP v2 — the v2 ASN.1 is no longer in
  TS 29.002 and was not checked. Version 2 dialogues opened by peers are still refused.

- Reassembly of segmented XUDT (Q.714 §4.1.1.2). A peer may send a long message — an HLR's answer,
  an MSC's MO-ForwardSM — as up to 16 XUDT segments; they were not understood. They are now
  reassembled before delivery, keyed by calling address, OPC and segmentation local reference;
  missing segments are given up after T(reass) = 15 s; a duplicate or inconsistent segment, a
  first segment for a busy process, a timeout, or more than 3952 octets fails the reassembly and,
  if the sender asked, returns the message in an XUDTS with the Q.713 cause. At most 1000
  reassemblies run at once (beyond that: XUDTS, network congestion). XUDTS is now encoded and
  decoded; a returned XUDT is reported like a returned UDT.

### Fixed

- A successful MT-ForwardSM answered without a result parameter — allowed: the result is optional
  (TS 29.002 §17.6.3), and version 2's forwardSM has none — failed to decode, and the caller timed
  out. An empty result is now a successful delivery.

## [0.7.2] - 2026-10-14

### Security

- Dialogue-table exhaustion: a dialogue the gateway answered with END (every MO-ForwardSM, every
  refused or rejected operation) stayed counted against `tcap.max_dialogues` until
  `tcap.dialogue_timeout` (30 s). Sustained traffic — or a peer sending BEGINs — filled the table,
  and new dialogues were refused. Sending END or ABORT now ends the dialogue at once (Q.774).
  0.7.1 and earlier are affected; upgrade.
- Dialogues opened by a peer's empty Begin are bounded: at most a tenth of `tcap.max_dialogues`
  may wait for the peer's CONTINUE, each for at most 5 s; beyond that the BEGIN is refused
  (P-ABORT, resource limitation).

### Changed (breaking for deployments)

- SendRoutingInfoForSM is now addressed the way an SMS-GMSC does it: the called party is the
  subscriber's MSISDN as a global title with SSN 6, routed on GT (through `sccp.gtt_rules`, to
  the STP), so the network finds the subscriber's HLR — number ranges and number portability
  included. Before, every SRI went to the one configured HLR. New `map.sri_routing`: `msisdn`
  (default) or `hlr` for the previous behaviour. An HLR named in the request still wins.
  `map.hlr_address` and `map.hlr_pc` are no longer required (`hlr_address` was only ever logged).

### Added

- Long messages. An operation too long for one SCCP unitdata — a full-length SMS in an
  MT-ForwardSM, for instance — was refused. The gateway now opens such a dialogue with an "empty
  Begin" (the dialogue portion alone) and sends the components in the first CONTINUE once the
  peer accepts (TS 29.002 §C.2.1); it accepts the same sequence from a peer (e.g. a long
  MO-ForwardSM), answering the empty Begin with a CONTINUE carrying the AARE. The size limit
  accounts for the MTP3 signalling information field (272 octets less the routing label) and the
  actual SCCP addresses. SCCP segmentation (XUDT) is deliberately not used to send: not every
  transit node carries it (§C.1).

### Fixed

- A message that never reached the peer left its caller waiting for `tcap.dialogue_timeout`
  (30 s by default): no SCCP route for its GT, M3UA unable to send it (association down), or the
  network returning it in a UDTS. Each now ends the dialogue at once, and the caller gets
  `ABORTED` (`error_code` -2) with the reason in `error_message` — e.g. "returned by the network:
  UDTS cause 1 (no translation for this specific address)" or "not sent: … no route to
  destination".

## [0.7.1] - 2026-10-14

### Security

- Remote denial of service: a malformed TCAP message, or a CAP InitialDP without its optional
  party numbers, could crash the gateway process. Both are fixed; a malformed message is now
  dropped, and a defect in any operation handler costs that message only. 0.7.0 is affected;
  upgrade to 0.7.1.

### Fixed

- The answer to a dialogue opened by a peer (MO-ForwardSM from an MSC) carried no dialogue
  portion. Its first END or CONTINUE now carries an AARE accepting the application context
  (Q.773 §4.2.1).
- A dialogue under an application context the gateway does not implement was refused with a
  P-ABORT (no-common-dialogue-portion). It is now refused the way TS 29.002 §12.1 expects:
  TC-U-ABORT, AARE reject-permanent / application-context-name-not-supported, naming the version
  the gateway supports so the peer can retry.
- A TC-U-ABORT from the peer (e.g. an HLR refusing the proposed version) could not be parsed —
  go-tcap's parser reads a P-Abort cause unconditionally — so the dialogue ran into its timer;
  had it parsed, the absent cause would have crashed the abort handler. ABORTs are now parsed
  with go-tcap's BER parser, and the caller gets `ABORTED` at once with the refused context and
  the peer's proposal in `error_message`.

## [0.7.0] - 2026-10-14

The first public release. Internal builds before it carried the
version 1.0.0 (container image and chart `appVersion`). That number overstated the gateway's
maturity and is not continued: versions stay below 1.0 until the gateway has carried traffic on
real SS7 networks. The chart moves from 0.3.x to 0.4.0 with this release (new required settings,
new image location).

### Added

- Licensed under the GNU Affero General Public License v3.0 only. A commercial licence is
  available from 3eAI Labs Ltd. Contributions are accepted under the 3eAI Labs Contributor
  Licence Agreement (`CLA.md`).
- The NATS wire contract (subjects, message schema, parameter keys, status and failure codes)
  now comes from the Apache-2.0 module `github.com/3eAI-Labs/ss7-3eai-gateway-client`, which also
  provides a typed Go client for SRI-for-SM, MT-Forward-SM and MO-Forward-SM. Wire format is
  unchanged.
- A MAP ReturnError from the peer is published on the operation's reply subject:
  `status=REMOTE_ERROR`, `error_code` = the MAP error code (3GPP TS 29.002 MAP-ErrorCodes),
  `error_message` = its name. A TCAP Reject is published the same way with `error_code=-3`.
  Before, both were only logged and the caller waited out its own deadline — an SMSC could not
  tell `absentSubscriberSM` (retry later) from `unknownSubscriber` (do not retry).
- MT-ForwardSM failures (timeout, abort) now carry `error_message`, as SRI-for-SM failures did.
- Licence gate in CI: SPDX headers on every source file, AGPL-compatible dependency licences,
  and third-party licence texts shipped in the container image.

### Documentation

- The README describes what the gateway does today, with a list of known limitations, instead
  of the original plan (which claimed a production-ready FS.11 firewall, TDR generation, INAP,
  Prometheus metrics and configuration auto-sync). New `docs/ARCHITECTURE_OVERVIEW.md`. The chart
  description and value comments no longer claim the firewall and TDR.
- Removed `QUICKSTART.md`, `docker-compose.yml`, `monitoring/prometheus.yml`, the NATS bridge
  shell script and the `compose-*`, `test-integration` and `test-chaos` make targets. They depended
  on untracked per-node config files and unused Postgres/Redis services, scraped a `/metrics`
  endpoint that does not exist, and tested leader failover in a design without a leader.

### Changed (breaking for deployments)

- The chart's default images are `ghcr.io/3eai-labs/ss7-3eai-gateway` and
  `docker.io/library/nats:2.10-alpine` (were `registry.3eai-labs.com/…`). Releases publish the
  image and the chart (`oci://ghcr.io/3eai-labs/charts/ss7-3eai-gateway`) to GHCR.

- **`m3ua.network_indicator` is required** (0 = international, 2 = national; Q.704 §14.2.2).
  It is written in every MTP3 routing label, and an STP discards a mismatch, so it has no default.
- **GT-routed messages need `sccp.gtt_rules`.** A rule maps a called-GT pattern to the DPC: with
  `ssn` 0 the message keeps routing on GT and goes to that point code (the STP) for translation
  there; with `ssn` > 0 it is routed on that point code and SSN. MT-Forward-SM, addressed to the
  VMSC GT, needs a rule; without one it is not sent and is logged as "no route to destination".
- `map.hlr_pc`, `cap.gsmscf_pc` and `sccp.local_point_code` must fit the network variant
  (14 bits for ITU). The chart's ITU defaults were 24-bit values and are now `0x0002`/`0x0003`.
- Only the short-message application contexts are accepted on incoming dialogues
  (shortMsgGateway, shortMsgMO-Relay, shortMsgMT-Relay, all v3). `networkLocUp` v1-v3 are refused.
- `/api/v1/ready` returns 503 `signalling_link_down` while the M3UA association is down.

### Fixed

- The container image now carries the licence and NOTICE of the Apache-2.0 client module compiled
  into it; the licence check now checks that module too (both skipped it by a path prefix).
- The binary reports its build version (`-X main.AppVersion`, set from the release tag) instead of
  a hard-coded `1.0.0`, in the start-up log and `/api/v1/health`.

- Every M3UA DATA was sent with the routing label fixed at connect time — OPC = the ASP
  identifier, DPC = 0, NI = 0, SLS = 0 — which no STP can route. Each message now carries the
  label SCCP computed. The SLS rotates for protocol class 0 and is stable per address pair for
  class 1, and the SCTP stream follows the SLS.
- SRI-for-SM and MT-Forward-SM were sent under the location-update application context; they now
  use `shortMsgGatewayContext-v3` and `shortMsgMT-RelayContext-v3` (3GPP TS 29.002 §17.3.3).
  MO-Forward-SM from an MSC, sent under `shortMsgMO-RelayContext-v3`, was refused and is now
  accepted.
- Global title translation never ran, and no SCCP address set the routing indicator, so a
  GT-routed message had no DPC.
- A lost M3UA association never came back: the reconnect closed every new association straight
  away. It now re-dials until the SGW answers, including at boot, which no longer fails when the
  SGW is unreachable.
- Incoming SCCP messages not addressed to this node were relayed; they are now refused with a
  UDTS. UDTS had a spurious protocol-class octet, was sent without the originator asking for it,
  and an incoming UDTS was handed to TCAP as data.
- A UDT with more than 255 octets of user data is refused instead of being sent with a wrapped
  length indicator.
