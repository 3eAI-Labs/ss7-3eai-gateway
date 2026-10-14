# ss7-3eai-gateway

An SS7 signalling gateway that lets applications send and receive SMS over MAP without speaking
SS7 themselves. Applications talk to it over [NATS](https://nats.io); it talks to the
signalling network over SIGTRAN (M3UA over SCTP) as an application server process behind a
signalling gateway (SG) or STP.

```
 SMSC / application                 ss7-3eai-gateway                        SS7 network
 ──────────────────   NATS   ┌──────────────────────────────┐  M3UA/SCTP  ┌──────────┐
  map.sri.request    ───────►│ MAP ─ TCAP ─ SCCP ─ M3UA     │────────────►│  SG/STP  │─► HLR, MSC
  map.mt.sms.request ───────►│                              │◄────────────│          │
  map.*.response     ◄───────│                              │             └──────────┘
  map.mo.sms         ◄───────└──────────────────────────────┘
```

Written in Go. Licensed under the [GNU AGPL v3.0](LICENSE) (only); the
[client module](https://github.com/3eAI-Labs/ss7-3eai-gateway-client) is Apache-2.0.

## Status

**Not production-ready, and not yet tested against real SS7 equipment.** The short-message path
below is implemented end to end and tested over real SCTP against a peer that checks the routing
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

- **Every outcome is answered.** The response subject carries `DELIVERED`, or a failure:
  `REMOTE_ERROR` with the MAP error code (e.g. `absentSubscriberSM` vs `unknownSubscriber`, which
  an SMSC retries differently), `TIMEOUT`, or `ABORTED`.
- **Signalling transport.** M3UA ASP over SCTP (optionally multi-homed), with override or
  loadshare traffic mode. Each message carries its own MTP3 routing label. A lost association is
  re-established automatically, including when the SG is not reachable at start-up.
- **SCCP.** Connectionless (UDT, protocol class 0), ITU (14-bit) or ANSI (24-bit) point codes. Called
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

**Protocol:**
- Only the three operations above. Other MAP operations, and dialogues under other application
  contexts, are refused. Only version 3 contexts are offered or accepted; there is no fallback to v2.
- SendRoutingInfoForSM is addressed to the configured HLR (`map.hlr_gt` / `map.hlr_pc`), not to
  the subscriber's MSISDN as a global title. It therefore does not work in networks that locate
  the HLR by number range or number portability.
- No SCCP segmentation (XUDT/LUDT): user data over 255 octets is refused.
- An incoming UDTS is logged with its cause, but the dialogue it belongs to ends only on its
  timer. A message SCCP cannot route (no matching rule) is logged, and the caller sees `TIMEOUT`.
- ANSI SCCP addresses use the ITU address-indicator layout; this is not verified against
  ANSI T1.112.
- CAP: an incoming InitialDP gets no answer, and CAP origination needs pre-encoded BER. The CAP
  application context names have not been verified. INAP is not supported.

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
| `map.hlr_gt` / `map.hlr_pc` | Where SendRoutingInfoForSM is sent |

Secrets (`api.jwt_secret`, the admin password) and per-node identity can come from environment
variables (`SS7_` prefix). See the [Helm chart](deploy/helm/ss7-3eai-gateway/README.md) for
Kubernetes. SCTP needs host networking there.

## Documentation

- [Architecture overview](docs/ARCHITECTURE_OVERVIEW.md)
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
