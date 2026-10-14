# Changelog

All notable changes to `ss7-3eai-gateway` are documented in this file.
Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versioning follows [SemVer](https://semver.org/).

## [Unreleased]

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
