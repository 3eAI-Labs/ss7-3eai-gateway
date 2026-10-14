# Changelog

All notable changes to `ss7-3eai-gateway` are documented in this file.
Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versioning follows [SemVer](https://semver.org/).

## [Unreleased]

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

- **M3UA reconnect after EISCONN.** A dial whose SCTP association came up while the connect call was
  being restarted after a signal returned EISCONN, and the SCTP library closed the association.
  The gateway then waited a full reconnect interval and the SGW was left holding a dead
  association. It now dials again at once (up to 3 times). This was seen once in CI; the cause is
  our reading of it and could not be reproduced locally. The test SGW also no longer stops
  accepting after one failed handshake.

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
