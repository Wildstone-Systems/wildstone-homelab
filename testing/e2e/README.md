# e2e harness (E0.2)

Boots disposable Ubuntu 24.04 VMs, will install Wildstone components onto
them, and asserts behavior. Network boundary is governed by
[ADR-W002](../../docs/decisions/ADR-W002-e2e-isolation-boundary.md):
hosted CI and cloud agents never reach Proxmox.

## Providers

- **local** — QEMU + cloud-init NoCloud seed on the current machine. Works
  on GitHub-hosted runners (KVM available) and anywhere with
  `qemu-system-x86_64`/`qemu-img` installed; falls back to TCG emulation
  without `/dev/kvm`. This is the CI path.
- **proxmox** — clones a cloud-init template via the Proxmox API.
  **Untested**: only usable from inside the tailnet, so its first real run
  must happen there (owner's machine or a self-hosted runner). See the
  header of `proxmox.go` for the template/token assumptions.

## Running

Unit tests run with plain `go test ./...`. The boot smoke test is opt-in:

```sh
curl -LO https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img
WILDSTONE_E2E_PROVIDER=local \
WILDSTONE_E2E_IMAGE=$PWD/noble-server-cloudimg-amd64.img \
  go test -run TestSmoke -timeout 30m ./...
```

In CI the smoke test runs via the manually-triggered `e2e` workflow
(`.github/workflows/e2e.yml`), not on every push.

The same manual workflow has a separate `TestEngine` QEMU job. It uses a
larger disposable guest, copies the exact checked-out signed-playbook source
and signing tool into it, installs k3s and Longhorn, then checks the repeat
install, storage, leader fence, and tamper refusal. It runs without a VIP:
QEMU user networking cannot provide the LAN address kube-vip needs. The
owner-operated VM evidence in #159 covers kube-vip and running Wardstone
leadership. The QEMU job does not yet install Keystone/Wardstone services.
