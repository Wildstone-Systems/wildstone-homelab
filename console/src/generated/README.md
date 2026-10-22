# console/src/generated/

Committed generated code (the `proto/gen` pattern). `resources.ts` is
emitted from the Go resource schema:

    cd keystone && go run ./resource/gen -out ../console/src/generated/resources.ts

CI regenerates and fails on drift (ADR-W007 A7). Do not edit by hand.
