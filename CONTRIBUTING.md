# Contributing

Thanks for taking the time to look at this. A few things about this project are
unusual, and knowing them up front saves a review cycle.

## Before you start

- **Open an issue first** for anything larger than a typo fix. Design decisions in
  this codebase are usually the result of a measurement, and a patch that ignores
  that measurement gets sent back.
- **One topic per pull request.** A refactor plus a behaviour change is two PRs.
- **No new third-party Go modules.** `controller/go.mod` deliberately has zero
  `require`s: the control plane builds in an air-gapped environment. If you need a
  library, say so in the issue first — it is a deliberate decision, not an oversight.
  The same goes for the console: no CDN, no framework, no build step.

## The two rules that shape everything

These come from `AGENTS.md` (written in Chinese); they are not style preferences:

1. **Nothing in the configuration path may restart a daemon.** FRR is updated with
   `frr-reload.py` computing a diff, VPP with incremental CLI/API calls; `sr-agent`
   hot-applies. A measured restart of FRR dropped 37.5% of packets and idled an eBGP
   session for ~4s. If a change cannot be applied without a restart, it is reported
   as a failed apply — never "restart and hope".
2. **Evidence, not exit codes.** Every change ships with before/after numbers: packet
   loss, FIB/RIB counts, neighbour uptime, pids. "Looks fine" is not a test result.
   If you add a hot path, answer one question in the PR: does this drop traffic that
   is already flowing?

## Development setup

```sh
cd controller
go build ./cmd/... && go vet ./... && go test ./...
```

- Go 1.22 or newer. The `internal/frr/krt` tests need `NET_ADMIN` when run in a
  container (`docker run --cap-add NET_ADMIN ...`); that is an environment limit,
  not a flaky test.
- The VPP plugin builds against VPP 26.06 headers (`cmake -G Ninja`); unit tests for
  the C code build standalone via `make -C vpp-plugin lib`.
- For end-to-end work use the container lab in `test/lab/` (see its README); do not
  test config compilation by hand-editing state files.

## Tests

- New behaviour needs a test that fails without the change. Rotation, ordering and
  parsing logic in this repo is covered by tests that were verified by mutation —
  break the implementation and the test must go red.
- Run `go test -race` for anything touching the controller's shared state
  (`sr-ctrl` handlers take `s.mu`; only `*Locked` helpers may be called while it is
  held).
- Console changes: `node --check` the extracted `<script>` block, and keep the
  Chinese and English string tables in sync (see below).

## Console strings (i18n)

The console ships two languages. Chinese strings are the source; English lives in
`controller/cmd/sr-ctrl/assets/console-i18n.js` as a dictionary plus a small set of
regex rules for strings assembled at runtime.

- Add a new Chinese string → add the English entry in the same PR.
- Do not translate: identifiers, `srou://` URIs, flag names, protocol names
  (BGP, VXLAN), or anything inside `<code>`/`<pre>`.
- Keep the glossary in that file's header intact (`入网授权` = claim grant,
  `导通策略` = steering policy, `中转` = relay, `台账` = ledger, …) so the UI stays
  consistent.

## Commit messages

Imperative subject, one line, no trailing period; body explains *why* and carries the
measurements. Sign off every commit:

```sh
git commit -s -m "sr-rr: rotate controller candidates after a failed fetch"
```

Sign-off certifies the [Developer Certificate of Origin 1.1](https://developercertificate.org/).
There is no CLA.

## Code layout expectations

- Go: `cmd/` wires flags and starts things; `internal/` holds the logic. Handlers may
  not contain business logic, and logic may not depend on `net/http` types.
- C: one node per file under `vpp-plugin/`, no allocation on the packet path, and any
  new counters go through `sr_obs.c` so `show sr-wg` keeps showing everything.
- Comments explain *why*, including the failure that motivated the code. This repo's
  comments are long on purpose; match that.

## Review

Expect questions about what you measured. That is the review, not an objection.
