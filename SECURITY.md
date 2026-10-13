# Security Policy

## Supported versions

Only the most recent release receives fixes. There are no long-term support branches.

## Reporting a vulnerability

**Do not open a public issue.**

Use GitHub's private vulnerability reporting:
[**Report a vulnerability**](https://github.com/3eAI-Labs/ss7-3eai-gateway/security/advisories/new).
The report stays private between you and the maintainers until a fix is published.

Useful things to include, roughly in order of how much they help:

- Version (container image tag or commit) and the affected component (M3UA, SCCP, TCAP, MAP,
  CAP, NATS bridge, REST API, Helm chart)
- What an attacker gains — the impact matters more than the mechanism
- A reproduction: a failing test, a PCAP, or the exact message sequence. Strip real subscriber
  identities (IMSI, MSISDN) and operator global titles before attaching captures
- Network variant (ITU or ANSI) and the relevant configuration, credentials removed

You will get an acknowledgement, and an honest estimate rather than a promised deadline. If a
report turns out to be a plain bug rather than a vulnerability, you will be told so and asked to
move it to a public issue.

## Scope

In scope — anything in this repository that lets an attacker:

- get the gateway to accept, route or originate signalling it should refuse: spoofed calling
  party addresses, SCCP/GTT misrouting, TCAP dialogues hijacked across transaction IDs
- crash the gateway or exhaust its state (dialogue table, invoke table, reassembly buffers) with
  malformed or crafted M3UA/SCCP/TCAP/MAP/CAP input
- read or alter subscriber data (IMSI, MSISDN, location, SMS content) that should not reach
  them, including through logs, metrics, NATS subjects or transaction detail records
- bypass authentication or authorisation on the REST API, or obtain its credentials
- obtain secrets from a deployment that followed the Helm chart's documented production settings

Out of scope:

- weaknesses of SS7 itself that the standards leave to network operators — the gateway is not a
  substitute for a signalling firewall at the network edge
- the NATS server, the Kubernetes cluster, or a deployment's own configuration — for example NATS
  without TLS or authentication, or an M3UA association exposed to untrusted networks
- the smoke/development defaults of the Helm chart (chart-generated secrets, bundled single-node
  NATS), which the chart documents as unsuitable for production
