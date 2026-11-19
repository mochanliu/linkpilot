# Security policy

## Reporting a vulnerability

Please **do not** open a public issue for a vulnerability.

- Preferred: use the repository's private vulnerability reporting / security advisory
  feature if the mirror you are reading this on offers one.
- Otherwise mail the maintainer at the address on the repository's owner profile, with
  `[linkpilot security]` in the subject.

Include the version (commit hash — the binaries carry no version flag yet), what you
did, what you observed, and the smallest reproduction you have. We will acknowledge
within a few days and tell you whether we consider it a vulnerability; please give us
a chance to ship a fix before publishing.

## What is in scope

- The device mTLS surface (`:8444`): enrollment, heartbeat, bundle fetch, RR config.
  Peer identity is a tenant-CA certificate whose URI carries role/tenant/id; a way to
  act as another device or another tenant is in scope.
- The console (`:8443`, `:8088` loopback): session handling, role checks, CSRF, and
  anything that lets a tenant reach another tenant's objects.
- Claim codes and enrollment: replay, cross-tenant use, use after revocation,
  exceeding the use count or the TTL.
- The VPP plugin: any packet that makes it allocate, loop, or write out of bounds,
  and any way to bypass the SR header integrity tag.
- The config compiler: a tenant-supplied value that escapes its intended field
  (command injection into `vtysh`, path traversal in uploaded files).

## Known and accepted limits (not vulnerabilities)

- A single claim code is a bearer token until it expires. TTL, use count and
  revocation are the controls; there is no second factor at enrollment.
- A claim code's envelope (controller address + tenant CA) is unauthenticated public
  data. Trust comes from the CA the device pins, not from the envelope.
- FEC and QoS shaping configured together is a documented product decision, not a bug.
- The controller's `-admin-token` is visible in `ps` on the controller host by design;
  protect the host, not the flag.

## Handling of secrets in this repository

No private keys, tokens or passwords are committed. Lab material under `test/lab/run/`
is generated locally and git-ignored. If you find a credential in the tree or in
history, treat it as a vulnerability and report it privately.
