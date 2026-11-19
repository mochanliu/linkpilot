# Third-party notices

This repository is Apache-2.0. The components below are either linked into what
we build, or their data/output is committed here. Each entry says exactly how it
enters the build, because that is what decides the obligation.

## 1. FD.io VPP — Apache License 2.0

- **What it is:** the packet-processing stack the data plane runs on.
- **How it enters the build:** `vpp-plugin/` `#include`s VPP headers
  (`vlib/`, `vnet/`, generated `vpp/api` bindings) and is compiled into a
  `sr_wg_plugin.so` that a VPP instance loads at runtime. We vendor **no** VPP
  source. VPP is a build and runtime prerequisite the operator installs.
- **Why our license must stay Apache-2.0-compatible:** Apache-2.0 code combined
  with our plugin must satisfy Apache-2.0 §4 (notices, license copy). MIT would
  also be compatible, but Apache-2.0 keeps the explicit patent grant and matches
  upstream, so the whole repository is Apache-2.0.
- **Upstream:** https://github.com/FDio/vpp — `LICENSE` (Apache-2.0).

## 2. SipHash-2-4 reference algorithm — CC0 1.0

- **What it is:** the keyed hash used for SR header integrity tags.
- **How it enters the build:** `vpp-plugin/sr_siphash.c` is our own C
  implementation written from the published algorithm description
  (Aumasson & Bernstein, "SipHash: a fast short-input PRF"), byte-explicit and
  endian-independent. No upstream source file is copied.
- **Status:** the reference algorithm and its sample code are released to the
  public domain (CC0 1.0). Attribution is given here as a courtesy.

## 3. Basemap data — public domain / ISC

- **What it is:** the world coastline shown on the console topology map.
- **How it enters the build:** `controller/tools/world-basemap/gen.mjs`
  (build-time only, needs node) reads six vendor files and writes
  `controller/cmd/sr-ctrl/assets/world-110m.js` and `world-50m.js`. The generated
  files are committed, so `go build` needs neither node nor network. The vendor
  inputs are not committed (see `controller/tools/world-basemap/README.md` and
  `SHA256SUMS`).

  | Input | License |
  | --- | --- |
  | Natural Earth 1:110m / 1:50m data (via world-atlas) | public domain data, ISC packaging |
  | `topojson-client` 3.1.0 | ISC |
  | `d3-geo` 3.1.1, `d3-array` 3.2.4 | ISC |

  At runtime the console loads only the committed path strings; no third-party
  JavaScript executes in the browser.

## 4. Go standard library — BSD 3-Clause

`controller/` has **zero** third-party module dependencies (`controller/go.mod`
has no `require` block). The Go standard library, BSD 3-Clause, is statically
linked into every binary we ship. Its license text ships with the Go toolchain;
binary redistributors should include it.

## 5. FRR — GPL-2.0 (external program, not distributed here)

The CPE drives FRR over `vtysh` / `frr-reload.py` to program BGP/OSPF and to
export the control-plane RIB. FRR runs as a **separate process** and is
communicated with over a socket; nothing from FRR is linked into our binaries and
no FRR source or binary lives in this repository.

Consequence for redistributors: if you ship an appliance image that contains both
this software and FRR, FRR keeps its own GPL-2.0 obligations (source offer for
FRR, and the FRR parts remain GPL-2.0). Our code stays Apache-2.0 because it is
not a derived work of FRR — but do not copy FRR sources into this tree.

## 6. Runtime prerequisites, not distributed

VPP, FRR, systemd and the Linux kernel are installed by the operator. Test
container images referenced under `test/` pull them from their upstream
registries at test time.

## 7. Design provenance — SR header inspiration (no license obligation)

The SR header's design approach is inspired by the expired Internet-Draft
[draft-zartbot-sr-udp](https://datatracker.ietf.org/doc/draft-zartbot-sr-udp/)
("Segment Routing over UDP (SRoU)", K. Fang et al., 2020, expired, never adopted
by an IETF working group). This entry is an acknowledgement, not a license
obligation: protocol ideas and wire formats are not copyrightable, and this
repository reproduces no text, figure or code from the draft — the format in
`docs/sr-header-format.md` and `vpp-plugin/` is an independent design.

## Reporting an omission

If you believe something in this tree is unattributed, open an issue or mail the
address in `SECURITY.md`; we will correct the notice.
