# Source and rights

The Cursor transport was extracted from
[AsterGateway/cliproxy-plugins](https://github.com/AsterGateway/cliproxy-plugins),
commit [`68b0aeeb03271829069462952d4af244728d9902`](https://github.com/AsterGateway/cliproxy-plugins/commit/68b0aeeb03271829069462952d4af244728d9902).

The upstream copyright and redistribution restriction are retained verbatim in
`LICENSE`. This standalone derivative is published at the maintainer's direction and does
not grant a new open-source license. The original upstream notice is retained
verbatim as provenance; upstream reuse restrictions continue to apply.

Derived files:

- `internal/cursor/auth.go`, `client.go`, `env.go`, `wire.go` and their original
  `client_test.go` / `wire_test.go` tests. UUID fixtures are normalized to a
  visibly synthetic value for publication; assertions are retained.
- `proto/cursor_v1.proto`: the text-chat subset of the upstream merged schema.
  Retained fields preserve their original names, types and wire numbers.
  Unsupported fields remain unknown fields to the protobuf runtime.
- `gen/cursorv1/cursor_v1.pb.go`: generated from that reduced schema.

The standalone HTTP adapter, configuration, deployment files and additional tests
are maintained in this repository. The CPA host ABI, other plugins, capture tools,
auth records and runtime data are not part of the extraction.
