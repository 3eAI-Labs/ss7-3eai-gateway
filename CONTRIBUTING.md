# Contributing

Thanks for considering a contribution.

## How this repository is published

Development happens in a private repository; this one is published as a full-tree snapshot per
release. That is why the history here is short and its commits do not correspond one-to-one with
the work that produced them.

It does not change how you contribute. Open issues and pull requests here as normal — they are read
and answered here. An accepted change is applied upstream and appears in the next published
snapshot, credited in `CHANGELOG.md`. Your commit will not appear verbatim in this repository's
history, so if attribution matters to you, say so in the pull request and it will be recorded in
the changelog entry.

## Where a change belongs

The NATS wire contract — subjects, the message schema, parameter keys, status and failure codes —
is owned by [ss7-3eai-gateway-client](https://github.com/3eAI-Labs/ss7-3eai-gateway-client)
(Apache-2.0). This gateway imports it. A change to the contract starts there, is released, and the
gateway then moves to the new version. Everything else — the protocol stack, the NATS bridge, the
REST API, the Helm chart — belongs here.

## Building and testing

Linux is required: the M3UA transport uses kernel SCTP through cgo.

```bash
sudo apt-get install -y libsctp-dev lksctp-tools   # Debian/Ubuntu
sudo modprobe sctp

go build ./...
go vet ./...
go test -race ./...                                       # unit tests
go test -tags integration -count=1 ./cmd/gateway/         # end-to-end, needs kernel SCTP
```

The end-to-end tests run the real boot path against an in-process SS7 peer over a loopback SCTP
association and an embedded NATS server. If your checkout sits inside a parent directory that has
a `go.work` file, run the commands with `GOWORK=off`.

`scripts/verify-chart-package.sh` checks the Helm chart package (needs `helm` and PyYAML), and
`scripts/check-licenses.sh` checks licence headers and dependency licences.

## Pull requests

Open an issue first for anything beyond a small fix, so the approach can be agreed before you spend
time on it.

- Keep the change focused; unrelated cleanups make review harder
- Add or update tests for behaviour changes; protocol changes need a test that encodes or decodes
  the exact octets, citing the specification clause (3GPP TS, ITU-T Q-series, IETF RFC)
- Match the style of the code you are editing rather than introducing a new one
- Write comments in English, and only where they explain something the code cannot say itself
- New Go files start with the licence header used by the existing ones
- Update `CHANGELOG.md` under `## [Unreleased]` for anything user-visible
- Accept the [contributor licence agreement](CLA.md) when the bot asks — once, on your first
  pull request

Never attach captures or logs that contain real subscriber identities (IMSI, MSISDN) or operator
global titles.

## Reporting bugs

Use the issue templates. Security vulnerabilities do **not** belong in a public issue. See
[SECURITY.md](SECURITY.md).

## Licence and contributor agreement

This project is released under the [GNU Affero General Public License v3.0](LICENSE) (AGPL-3.0-only),
and every contribution is published under it.

Before a contribution can be merged you also have to accept the
[3eAI Labs Contributor Licence Agreement](CLA.md). You keep the copyright in what you write; the
agreement grants 3eAI Labs the right to distribute your contribution, including under licence terms
other than the AGPL — 3eAI Labs also offers the gateway under a commercial licence. Read section 2
before accepting.

Accepting takes one pull request comment. A bot asks for it on your first pull request:

```
I have read the CLA Document and I hereby sign the CLA
```

You do this once, and it covers ss7-3eai-gateway-client as well. Without it the pull request
status check stays red and the contribution cannot be merged.

If you are contributing in the course of your employment, say so in the pull request — see section
4.2 of the agreement.
