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
  --go_out=. --go_opt=module=github.com/Wildstone-Systems/wildstone-homelab/proto \
  --go-grpc_out=. --go-grpc_opt=module=github.com/Wildstone-Systems/wildstone-homelab/proto \
  wardstone/v1/wardstone.proto
```

`protoc` is a C++ binary and is often not installable where the code is.
`buf` is pure Go, so `go install` reaches it anywhere the module proxy
does, and it drives the same plugins:

```sh
go install github.com/bufbuild/buf/cmd/buf@latest
cd proto && buf generate --path wardstone/v1/wardstone.proto --template \
  '{"version":"v1","plugins":[{"plugin":"go","out":".","opt":["module=github.com/Wildstone-Systems/wildstone-homelab/proto"]}]}' .
```

Pass the DIRECTORY as the input and select the file with `--path`. Given
the file as the input, buf roots it at its own parent, and the generated
code registers the descriptor as `wardstone.proto` instead of
`wardstone/v1/wardstone.proto` — renaming every internal symbol and
changing the name the file occupies in `protoregistry`. Add
`protoc-gen-go-grpc` to the plugin list only when a `service` changed;
regenerating the grpc file otherwise is pure diff noise.

Either way the version line in the header records the generator that ran,
so a `protoc (unknown)` in a diff means buf produced it.
