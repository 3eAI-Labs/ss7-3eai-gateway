# ss7-3eai-gateway — Helm chart (Arinna-onboardable)

Packages the SS7 3eAI Gateway for **Arinna onboarding** per the Arinna Product Packaging
Contract v2.1. What the gateway does and does not implement is in the
[project README](../../../README.md#known-limitations).

- **Intake:** chart-only (no CRD). The gateway's day-2 lifecycle is a rolling
  update + REST/NATS config sync — HA is NATS-JetStream-KV with all nodes equal
  (no leader election, `internal/raft/` is empty), so no custom reconcile loop
  is needed. Per §3, that is **chart-only**, and onboarding runs **smoke-install**
  (not smoke-upgrade; §4 CRD lifecycle does not apply).
- **Machine API:** [`values.schema.json`](./values.schema.json) — typed, complete,
  with `x-arinna` metadata marking the Arinna-tunable subset (§2, §2.1).
- **Registry:** image `ghcr.io/3eai-labs/ss7-3eai-gateway`, chart
  `oci://ghcr.io/3eai-labs/charts/ss7-3eai-gateway`, bundled NATS from
  `docker.io/library/nats`. Arinna's default allowlist (§8.1) must include
  `ghcr.io/3eai-labs` and `docker.io/library/nats`, or override `image.repository`
  and `nats.image` with an allowlisted mirror.

## Quick start (local / smoke)

```bash
# Self-contained: bundles a single-replica NATS JetStream and generates random
# dev secrets at install (kept across upgrades; read admin-password from the
# Secret — see NOTES).
helm install smoke deploy/helm/ss7-3eai-gateway -n ss7-smoke --create-namespace
kubectl -n ss7-smoke rollout status statefulset/smoke-ss7-3eai-gateway
```

## Production

```bash
helm install ss7 deploy/helm/ss7-3eai-gateway -n ss7 --create-namespace \
  --set hostNetwork=true \                              # SCTP needs host net
  --set replicaCount=3 \
  --set nats.deploy=false --set-string nats.url=nats://platform-nats:4222 \
  --set secrets.create=false --set-string secrets.existingSecret=ss7-secrets \
  --set-string stickyGtBase=905300000001 \             # per-node sticky CgPA GT
  --set persistence.enabled=true --set podDisruptionBudget.enabled=true \
  --set-string config.sccp.cluster_global_title.address=905300000000 \
  --set-string config.m3ua.sgw_address=10.0.0.10
```

`secrets.existingSecret` must carry keys: `jwt-secret`, `admin-password`,
`tdr-hmac-secret`, and (optional) `aws-access-key-id` / `aws-secret-access-key`. (The TDR
and AWS keys are read but not yet used: no transaction data records are generated.)

The network-specific settings have no usable defaults and must be set for a real network:
`config.m3ua.network_indicator` (0 international, 2 national), `config.m3ua.routing_context`,
`config.sccp.local_point_code`, and `config.sccp.gtt_rules` — at least
`[{pattern: "*", point_code: <STP point code>}]` so GT-routed messages (MT-ForwardSM) can leave.

## How the moving parts satisfy the contract

| Concern | Mechanism |
|---|---|
| Single intake envelope (§1) | This Helm chart; images on `ghcr.io/3eai-labs` and `docker.io/library` (§8.1 allowlist) |
| PSA `restricted` compliance (§1.4, v2.1) | Every container (gateway + bundled NATS) pins `allowPrivilegeEscalation:false` + `capabilities.drop:[ALL]` + `seccompProfile:RuntimeDefault`; pods run `runAsNonRoot` — required or the `arinna-smoke` namespace admission-rejects all pods |
| Typed surface (§2) | `values.schema.json`, `additionalProperties:false`, validated by `helm lint` |
| Exposure boundary (§2.1) | `x-arinna` blocks; un-annotated fields keep chart defaults. Every `userExposed:true` field declares `valueType` (v2.1 — the PD generator fails without it); preset-managed `resources` sizing is `userExposed:false` (sanctioned lever: `serviceLevelPreset`) |
| Capability contract (§2.2, ADR-032) | Schema-root `arinna-capabilities` **provides** block: `ss7-map-mo-mt` (governed registry id) — subscribes `map.{sri,mt.sms}.request`, responds `map.{sri,mt.sms}.response` + `map.mo.sms` (MO inbound). Also `ss7-map-location`, `ss7-map-call-routing`, `ss7-map-ussd`, `ss7-map-mobility` and `ss7-map-authentication` (registry entries requested). Visibility-only for now; lets Arinna match consumer `requires` (e.g. SMSC) at onboarding |
| Secret roles (§2.1, ADR-036) | `secrets.existingSecret` is the `secretRef`; jwt/hmac/admin/AWS are `secretValue` and **empty in chart defaults** (`ARIN-ONB-0024`). Injected as `SS7_*`/`AWS_*` env from a Secret — **never** in the ConfigMap. Smoke/dev (`create=true`): empty values are generated once and reused on upgrade via `lookup`; the literals published by chart ≤ 0.2.0 are rejected if passed and rotated if found in a live Secret |
| Per-node identity / sticky-CgPA (ADR-006) | StatefulSet ordinal → `SS7_NODE_ID*` + `stickyGtBase`+N via the container command wrapper |
| Declarative day-2 (§5) | `opsTrigger.{reloadConfig,resetPeer,drain,flushTDR}` values → ConfigMap token; outcome via K8s Event (RBAC in `rbac.yaml`) |
| Smoke-installable (§7.4) | Smoke-safe defaults: 1 replica, bundled NATS, `tdr.enabled=false`, M3UA `reconnect_max_attempts=0`. Boot does not wait for the SGW; without one the pod runs but stays not-ready (`/api/v1/ready` → 503 `signalling_link_down`), which the install-smoke verdict does not depend on |
| Write-class safety (§6) | SHACL bundle packaged **inside the chart** at [`shapes/`](shapes) (`ss7-3eai-gateway.shacl.ttl` + `ss7-3eai-gateway-ontology.ttl`) — Arinna reads only the chart at onboarding. Repo-root `shapes` is a symlink to it. Guarded by `scripts/verify-chart-package.sh` (CI `chart` job) |

### Env-injected keys (Viper `SS7_` prefix, dots→underscores)

The gateway binds these keys to env (`cmd/gateway/main.go` `envOverridableKeys`);
env overrides the file so the ConfigMap stays secret-free and per-pod identity works:

`SS7_NODE_ID`, `SS7_NODE_ID_NUMERIC`, `SS7_TCAP_NODE_ID`,
`SS7_SCCP_LOCAL_GLOBAL_TITLE_ADDRESS`, `SS7_API_JWT_SECRET`,
`SS7_API_DEFAULT_ADMIN_PASSWORD`, `SS7_TDR_HMAC_SECRET`, `SS7_NATS_URL`.

## opsTrigger — implemented

Both sides are wired. `opsTrigger.*` values render into the ConfigMap as tokens,
mounted live (non-`subPath`) at `/etc/ss7-gateway/opstrigger.yaml`. The in-process
consumer (`internal/opstrigger`, started from `cmd/gateway` `startServices`) polls
the file (15s), **baselines tokens at startup** (a fresh pod does not re-run a
pre-existing token), and on a changed non-empty token runs the action **once**
(idempotency keyed on the token; a failed action is reported and only retried by a
new token). Each outcome is reported on the standard surfaces (§5): always the
process log, plus a **K8s Event** when in-cluster —
`reason=OpsTriggerCompleted|OpsTriggerFailed`,
`message="action=<a> token=<echo> result=success|failed"`, `involvedObject`=the
pod. The Event is posted over `net/http` with the pod's service-account token +
cluster CA (no client-go dependency); the ServiceAccount RBAC is in `rbac.yaml`.

| Action | Effect |
|---|---|
| `reloadConfig` | Re-read + validate the on-disk config (bad mount ⇒ `OpsTriggerFailed`). Live re-apply to running handlers not yet supported — config changes that must take effect roll the pod via the config checksum. |
| `resetPeer` | Drop the M3UA/SCTP association; the association supervisor re-establishes it — the same path as a real link flap. |
| `drain` | Fail readiness (`/api/v1/ready` → 503) so K8s stops routing new traffic, and `NATS.PauseIntake()` stops new externally-originated dialogues; liveness (`/api/v1/health`) stays up and in-flight dialogues finish. |
| `flushTDR` | Flush the TDR buffer to S3. No records are generated yet, so this has nothing to flush. |

Probes: **liveness** → `/api/v1/health`, **readiness** → `/api/v1/ready` (503 while
draining or while the M3UA association is down). Path override: `--opstrigger-file` (default `<config dir>/opstrigger.yaml`).

## Conformance checklist (§11)

- [x] Helm chart (single intake envelope)
- [x] Pod Security `restricted` compliant — every container carries the §1.4 securityContext triple (v2.1)
- [x] `valueType` on every `userExposed:true` field (§2.1, v2.1)
- [x] `arinna-capabilities` `provides: ss7-map-mo-mt` declared (§2.2, optional)
- [ ] Chart + images on an allowlisted registry — published on `ghcr.io/3eai-labs`; Arinna allowlist entry requested
- [x] Secret roles declared — `secretRef` on `existingSecret`, `secretValue` (empty default) on every credential field (§2.1, ADR-036)
- [x] `values.yaml` defaults present
- [x] `values.schema.json` typed, complete, validatable
- [x] `x-arinna` on operator-tunable fields
- [n/a] Operator/CRD — chart-only product (§3)
- [x] Imperative day-2 modeled as declarative triggers (consumer implemented — `internal/opstrigger`, token idempotency + K8s Event outcomes)
- [x] Installable in isolated namespace with sane defaults (smoke-install)
- [x] SHACL shape bundle authored and packaged in the chart (`shapes/`, §6)
