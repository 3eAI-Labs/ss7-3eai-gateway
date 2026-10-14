# ss7-3eai-gateway

An SS7 signalling gateway that lets applications send and receive SMS, ask where a subscriber
is, and ask how to route a call to them, over MAP without speaking SS7 themselves. Applications talk to it over [NATS](https://nats.io); it talks to the
signalling network over SIGTRAN (M3UA over SCTP) as an application server process behind a
signalling gateway (SG) or STP.

```
 SMSC / application                 ss7-3eai-gateway                        SS7 network
 ──────────────────   NATS   ┌──────────────────────────────┐  M3UA/SCTP  ┌──────────┐
  map.sri.request    ───────►│ MAP ─ TCAP ─ SCCP ─ M3UA     │────────────►│  SG/STP  │─► HLR, MSC
  map.mt.sms.request ───────►│                              │◄────────────│          │
  map.ati.request    ───────►│                              │             │          │
  map.psi.request    ───────►│                              │             │          │
  map.routing.request ──────►│                              │             │          │
  map.*.response     ◄───────│                              │             └──────────┘
  map.mo.sms         ◄───────└──────────────────────────────┘
```

Written in Go. Licensed under the [GNU AGPL v3.0](LICENSE) (only); the
[client module](https://github.com/3eAI-Labs/ss7-3eai-gateway-client) is Apache-2.0.

## Status

**Not production-ready, and not yet tested against real SS7 equipment.** The operations below are
implemented end to end and tested over real SCTP against a peer that checks the routing
label, SCCP addressing and MAP application contexts the way an STP and an HLR would. That is
evidence of conformance, not proof of interoperability. Read [Known limitations](#known-limitations)
before connecting it to a network. Versions stay below 1.0 until the gateway has carried traffic on
real SS7 networks.

## What it does

| Operation (3GPP TS 29.002) | Direction | NATS subjects | Application context |
|---|---|---|---|
| SendRoutingInfoForSM (op 45) | gateway → HLR | `map.sri.request` → `map.sri.response` | shortMsgGatewayContext-v3 |
| MT-ForwardSM (op 44) | gateway → serving MSC | `map.mt.sms.request` → `map.mt.sms.response` | shortMsgMT-RelayContext-v3 |
| MO-ForwardSM (op 46) | MSC → gateway | published on `map.mo.sms` | shortMsgMO-RelayContext-v3 |
| AnyTimeInterrogation (op 71) | gateway (as gsmSCF) → HLR | `map.ati.request` → `map.ati.response` | anyTimeInfoEnquiryContext-v3 |
| ProvideSubscriberInfo (op 70) | gateway (in the HLR's role) → VLR | `map.psi.request` → `map.psi.response` | subscriberInfoEnquiryContext-v3 |
| SendRoutingInfo (op 22) | gateway (as GMSC) → HLR | `map.routing.request` → `map.routing.response` | locationInfoRetrievalContext-v3 |

- **Every outcome is answered.** The response subject carries the result, or a failure:
  `REMOTE_ERROR` with the MAP error code (e.g. `absentSubscriberSM` vs `unknownSubscriber`, which
  an SMSC retries differently), `TIMEOUT`, `ABORTED`, or `REJECTED` for a request the gateway
  refused before starting a dialogue.
- **Location and state.** AnyTimeInterrogation and ProvideSubscriberInfo return the cell, service
  area or location area, the serving VLR/MSC or SGSN, the age of the location (or a fresh one, by
  paging), the subscriber state, the EPS location, IMEI, MS classmark and number portability
  information, decoded into JSON.
- **Call routing.** SendRoutingInfo, a basic call interrogation, returns the IMSI and the roaming
  number (MSRN) to route the call on, or the forwarded-to number and forwarding options, with the
  serving MSC, number portability status, basic service and active supplementary services. An
  answer the HLR splits into TC-Result-NL segments is reassembled into one.
- **Signalling transport.** M3UA ASP over SCTP (optionally multi-homed), with override or
  loadshare traffic mode. Each message carries its own MTP3 routing label. A lost association is
  re-established automatically, including when the SG is not reachable at start-up.
- **SCCP.** Connectionless (UDT; protocol class 0 sent, the in-sequence delivery of class 1
  kept on receipt), ITU (14-bit) or ANSI (24-bit) point codes. Called
  addresses route on DPC/SSN or on global title. GT-routed messages are translated by configured
  rules, usually to the STP, which translates again.
- **Active-active cluster.** Nodes share a NATS queue group and a cluster GT. Each node has its own
  GT as calling address, so the network returns a dialogue's answers to the node that started it.
  TCAP transaction IDs carry the node number. No leader election and no shared dialogue state.
- **Operations.** REST API (JWT), liveness and readiness probes (readiness follows the M3UA
  association), and declarative day-2 actions (`resetPeer`, `drain`, …) through a watched file.
  A Helm chart is included.

The NATS message schema, subjects, status values and error codes are defined by the
[client module](https://github.com/3eAI-Labs/ss7-3eai-gateway-client), which also provides a typed
Go client. Applications in other languages implement the same JSON contract.

## Known limitations

What is planned for each of these is in the [roadmap](docs/ROADMAP.md).

**Protocol:**
- Only the operations above. Other MAP operations, and dialogues under other application
  contexts, are refused; an Invoke of an operation the gateway does not serve is rejected
  (unrecognizedOperation). The gateway only asks AnyTimeInterrogation, ProvideSubscriberInfo and
  SendRoutingInfo: peers cannot open those contexts with it.
- SendRoutingInfo is a basic call interrogation from a GMSC that does not offer CAMEL, OR, CUG,
  pre-paging or a second service (SCUDIF); interrogation for forwarding is not offered. Version 3
  only: there is no fallback to locationInfoRetrieval version 2. A result in TC-Result-NL
  segments is reassembled for SendRoutingInfo only; for other operations it is answered with
  `REMOTE_ERROR`.
- Of a location answer, the GPRS MS class, T-ADS data, time zone, 5GS location, CSG and LSA
  information and PDP context lists are not decoded. E-UTRAN cell and tracking area identities,
  routing area identity and geographical / geodetic information are passed through as hex. The gateway offers version 3 and falls back to version 2 when a peer
  refuses it (TS 29.002 §5.2.1); dialogues a peer opens under version 2 (an old MSC's forwardSM)
  are refused with an AARE proposing version 3.
- A message too long for one SCCP unitdata (a full-length SMS, for instance) is sent with a TCAP
  "empty Begin" (TS 29.002 §C.2.1), and accepted that way from peers. Segmented XUDT from a peer
  is reassembled; the gateway never segments with XUDT itself (not every transit node carries
  XUDT, §C.1). LUDT is not supported.
- ANSI SCCP addresses use the ITU address-indicator layout; this is not verified against
  ANSI T1.112.
- TCAP messages from peers must use definite lengths; the indefinite form (X.209) is not
  understood. Operation and error codes must be local values (global OBJECT IDENTIFIER codes are
  not used by MAP).
- CAP: an incoming InitialDP gets no answer, and CAP origination needs pre-encoded BER. The CAP
  application context names have not been verified. INAP is not supported.

**Access:** whoever may publish on `map.ati.request`, `map.psi.request` or `map.routing.request`
can locate any subscriber the network will answer for, or learn their IMSI and serving MSC. The
gateway does not authorise requests; restrict those subjects with NATS permissions, and remember
that `map.*.request` or `map.>` grants all of them. Response subjects are broadcast, so subscribe
rights on them show every requester's answers. (The gateway does fix the gsmSCF and GMSC
addresses itself, so a publisher cannot pass as another gsmSCF or GMSC.)

**Management plane:**
- **Not implemented on the message path:** the SS7 firewall (`internal/firewall`) and transaction
  data records (`internal/tdr`). They start and stop with the gateway, but no traffic passes
  through them.
- **REST rules are stored, not applied.** Rules added over REST (`/gtt/rules`, `/routes`,
  `/firewall/rules`) are stored in NATS KV but not applied. Translation rules take effect from the
  configuration file (`sccp.gtt_rules`).
- **Unimplemented endpoints.** These return `501`: `PUT /config`, `POST`/`DELETE /users`,
  `DELETE /ha/peers` and `POST /tdr/search`.
- **Users are node-local.** They are held in memory, so a token issued by one node is not
  accepted by another.
- **Metrics.** `/api/v1/metrics` does not yet serialise its counters, and there is no Prometheus
  endpoint.
- **Health status is constant.** `/api/v1/health` always reports `healthy`; use `/api/v1/ready`
  for link state.
- **`reloadConfig` only validates.** It re-reads and validates the configuration; changes take
  effect on restart.

## Building and testing

Requires Go 1.24 and, on Linux, the SCTP headers (`libsctp-dev`).

```bash
make build          # bin/ss7-gateway
make test           # unit tests with the race detector
make test-e2e       # end-to-end over real SCTP (Linux, kernel SCTP: sudo modprobe sctp)
make docker-build
```

The end-to-end suite starts an embedded NATS server, an in-process SS7 peer and one to three
gateway nodes. It covers SRI-for-SM, MT-ForwardSM, MAP errors, timeouts, the three-node cluster,
and losing and regaining the M3UA association; see [test/integration](test/integration/README.md).

## Running

The gateway needs a NATS server (JetStream enabled) and an SG or STP to connect to. Start from
[`config.example.yaml`](config.example.yaml):

```bash
./bin/ss7-gateway -config config.yaml
```

The settings that must match the network:

| Key | Meaning |
|---|---|
| `m3ua.sgw_address`, `sgw_port`, `routing_context`, `asp_id`, `traffic_mode` | The association and AS this node joins |
| `m3ua.network_indicator` | MTP3 NI in every routing label (0 international, 2 national). Required; an STP discards a mismatch |
| `sccp.network_variant` | `itu` (14-bit point codes) or `ansi` (24-bit) |
| `sccp.local_point_code`, `local_subsystems` | This node's point code and subsystems |
| `sccp.local_global_title`, `cluster_global_title` | Per-node calling GT and the cluster's shared GT |
| `sccp.gtt_rules` | Where GT-routed messages go: `{pattern: "*", point_code: <STP>}` sends them to the STP, which translates again; a non-zero `ssn` makes it a final translation |
| `map.sri_routing` | `msisdn` (default): SendRoutingInfoForSM goes to the subscriber's MSISDN as a GT, through `sccp.gtt_rules` to the STP; `hlr`: to `map.hlr_gt` / `map.hlr_pc` |

Secrets (`api.jwt_secret`, the admin password) and per-node identity can come from environment
variables (`SS7_` prefix). See the [Helm chart](deploy/helm/ss7-3eai-gateway/README.md) for
Kubernetes. SCTP needs host networking there.

## Documentation

- [Architecture overview](docs/ARCHITECTURE_OVERVIEW.md)
- [Roadmap](docs/ROADMAP.md): 0.7.x MAP, 0.8.x CAP, 0.9.x INAP
- [Client module and wire contract](https://github.com/3eAI-Labs/ss7-3eai-gateway-client)
- [Helm chart](deploy/helm/ss7-3eai-gateway/README.md)
- [Changelog](CHANGELOG.md)

## Licence

The gateway is licensed under the [GNU Affero General Public License v3.0 only](LICENSE). If you
run a modified version as a network service, the AGPL requires you to offer its source to the
users of that service.

**A commercial licence** without the AGPL's obligations is available from 3eAI Labs Ltd.

The [client module](https://github.com/3eAI-Labs/ss7-3eai-gateway-client) is Apache-2.0, so
applications that link it are not affected by the AGPL; they reach the gateway only over NATS.

Contributions are welcome under the [3eAI Labs Contributor Licence Agreement](CLA.md); see
[CONTRIBUTING.md](CONTRIBUTING.md). To report a vulnerability, see [SECURITY.md](SECURITY.md).

NATS is a Cloud Native Computing Foundation project. This gateway is an independent
implementation and is not affiliated with or endorsed by the NATS project, the CNCF, 3GPP, ITU-T
or GSMA.
