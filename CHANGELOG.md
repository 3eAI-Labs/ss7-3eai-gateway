# Changelog

All notable changes to `ss7-3eai-gateway` are documented in this file.
Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versioning follows [SemVer](https://semver.org/).

## [Unreleased]

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
