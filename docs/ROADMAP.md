# Roadmap

Where the gateway is going, one application protocol per minor line. There are no dates: a
release ships when it meets the bar below. What is implemented today, and what is not, is in the
[README](../README.md#known-limitations).

| Line | Protocol | Scope |
|---|---|---|
| **0.7.x** | MAP (3GPP TS 29.002) | Short messages first, then location, call routing, USSD, mobility |
| **0.8.x** | CAP (3GPP TS 29.078, CAMEL) | Prepaid call control and charging |
| **0.9.x** | INAP (ITU-T Q.1218 / ETSI CS) | Intelligent-network service control |
| **1.0.0** | — | The gateway has carried traffic on a real SS7 network |

Versions stay below 1.0 until the last row holds. Before 1.0, a minor line is one protocol and
each patch release in it adds one functional area; a fix that cannot wait takes the next number.

## What every functional area ships with

An area counts as delivered only with all of these:

1. **Application contexts.** Each operation is originated and accepted under the context TS 29.002
   (or 29.078, Q.1218) assigns it, and that context is registered with the dispatcher. A context
   the gateway does not implement is refused with an AARE naming what it does support.
2. **NATS contract.** Subjects, message fields, status and error codes are defined in the
   [client module](https://github.com/3eAI-Labs/ss7-3eai-gateway-client), with a typed Go method.
   A contract change is a client release first.
3. **Every outcome answered.** The caller gets a result, the peer's error code, or a failure
   (timeout, abort, not delivered) — never silence.
4. **End-to-end test against the strict peer.** The test goes over real SCTP, against a peer that
   checks the routing label, the SCCP addressing and the application context, and that answers
   the way a real node would, including its errors.
5. **Fuzzed decoders.** Every decoder that reads peer input has a fuzz test.
6. **Documentation.** README, architecture overview and CHANGELOG are updated.

## 0.7.x — MAP

| Release | Area | Operations | State |
|---|---|---|---|
| 0.7.0 | SMS core | SendRoutingInfoForSM, MT-ForwardSM, MO-ForwardSM | released |
| 0.7.1 | Security fix | Remote denial of service | released |
| 0.7.2 | SMS completion, part 1 + security fix | SRI-for-SM addressed to the subscriber's MSISDN; non-delivery reported at once; long messages by TCAP "empty Begin" (TS 29.002 §C.2.1) in both directions; dialogue-table exhaustion fixed | released |
| 0.7.3 | SMS completion, part 2 | Reassembly of incoming XUDT segments; fallback to version 2 when a peer refuses version 3 (accepting version 2 dialogues opened by peers is not included) | released |
| 0.7.4 | Location | AnyTimeInterrogation (op 71) towards the HLR; ProvideSubscriberInfo (op 70) | released |
| 0.7.5 | Call routing | SendRoutingInfo (op 22) towards the HLR | planned |
| 0.7.6 | USSD | processUnstructuredSS-Request (op 59), unstructuredSS-Request (60), unstructuredSS-Notify (61) | planned |
| 0.7.7 | Mobility and subscriber data | UpdateLocation (2), CancelLocation (3), ProvideRoamingNumber (4) — the gateway's role in these, application or network-node emulation, is decided when the area starts | planned |

## 0.8.x — CAP

- Verified CAP application context names.
- Answers to InitialDP: Continue, Connect, ReleaseCall.
- Event and charging control: RequestReportBCSMEvent, EventReportBCSM, ApplyCharging and
  ApplyChargingReport — enough for a prepaid call.
- CAP origination with typed requests, instead of the pre-encoded BER accepted today.

## 0.9.x — INAP

- Service control for intelligent-network services, using the same approach as CAP.

## Across all lines

These are scheduled into whichever release needs them first, and are required before 1.0:

- **Observability:** Prometheus metrics; a health endpoint that reflects component state.
- **Security:** an SS7 firewall on the message path (GSMA FS.11 categories).
- **Records:** transaction data records.
- **Configuration:**
  - translation rules managed over the REST API and applied live;
  - persistent, cluster-wide users;
  - configuration reload without restart.
- **ANSI:** the address-indicator layout verified against ANSI T1.112.
