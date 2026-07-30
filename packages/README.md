# packages/

Capability packages (data, not code) and the package build/lint/sign
toolchain. A package defines a profile of a capability: templates that expand
into typed resources, dependency traits, policies, lifecycle hooks (run as
containers with declared mounts — never host scripts), and the `agentTools`
declaration (ADR-W001).

Launch packages are transcriptions of proven configs from the reference
deployment — see `testing/fixtures/`.
