# proto/

The contract layer: gRPC service definitions (Keystone ↔ Wardstone, client ↔
Keystone) and the resource schemas (Container, Route, Volume, OidcClient,
BackupPlan, …).

Rules:

- Schemas are versioned from day one; this directory freezes shape at the
  Phase-0 exit gate (locked decisions #1–#3).
- The Wardstone protocol is a **closed enum of typed operations** — adding
  any generic execution primitive is a rejected change by policy, not taste.
- Go and TypeScript clients are generated from here; nothing hand-writes a
  wire type.
- The package format v1 includes the `agentTools` declaration block
  (ADR-W001) — parsed and validated at MVP, executed in v2.

## Layout

`wardstone/v1/wardstone.proto` is the Wardstone protocol (ADR-W003).
Generated Go is committed under `gen/` so builds never need protoc.

## Regenerating

Only needed when a .proto changes:

```sh
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
cd proto && protoc \
  --go_out=. --go_opt=module=github.com/chickenrat17/wildstone-homelab/proto \
  --go-grpc_out=. --go-grpc_opt=module=github.com/chickenrat17/wildstone-homelab/proto \
  wardstone/v1/wardstone.proto
```
