# Development

Use Go 1.26+ in an environment where you are authorized to build and test. The
application needs only the protobuf runtime; avoid adding a CPA or framework
dependency to the standalone HTTP adapter. Modify `go.mod` and `go.sum` through
the Go toolchain. Commit generated protobuf Go code so normal builds do not depend
on local generation tools.

Run `make fmt-check`, `make test`, `make vet` and `make build` before proposing
changes. Preserve the original transport regression tests. For protocol changes,
verify the request and response wire fields against the source schema and use a
synthetic HTTP upstream to exercise the full path. Never use production tokens,
raw account captures or private logs as fixtures. Confirm secrets are absent from
the committed files and Docker build context.

The optional `ci/github-actions.yml.example` template is not enabled by default.
A maintainer with workflow write permission can install it under `.github/workflows/ci.yml`.
It supports manual runs on a dedicated self-hosted Linux runner with the `cursor-proxy` label. The runner needs Git, Go, Make and a C compiler
for race-detector tests, provisioned by its administrator. This project does not
install runners or credentials. Do not enable untrusted PR execution on a runner
with access to other project data.

Retain the source attribution and original redistribution restrictions in
`NOTICE.md` and `LICENSE`. Publication of the source does not change those terms.
