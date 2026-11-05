# Isolated Flux-managed Keystone proof

Status: accepted synthetic operational evidence for #317 under #166 on
2026-11-05. This is a sanitized record. Private deployment identifiers,
credentials, source refs, endpoint details, claim/PV identities, and retained
evidence hashes are intentionally absent.

## Scope and tested artifacts

The exercise used one fresh, isolated namespace and one dedicated read-only
Git source. The source was reachable by source-controller before the proof
Keystone existed, so reconciliation did not depend on the management API or
its publisher. No direct Helm release or second reconciler owned the proof.

Tested versions and public artifact identities:

- K3s/Kubernetes `v1.37.1+k3s1` on one amd64 node;
- Flux distribution `v2.10.0`, with source-controller `v1.10.1`,
  kustomize-controller `v1.10.1`, and helm-controller `v1.7.1`;
- Helm renderer `v4.3.1`, binary SHA-256
  `b66cb19f2b31f9d427632ea6c4b4a1ff89b74ecaeaee323cd166da41a7af53bd`;
- `wildstone-management` chart `0.2.1` from public source commit
  `f13813cb`, chart tree
  `4c96219c3f06c39b24a174ab74e6d9110b5062ba`;
- locally built synthetic management image
  `docker.io/library/wildstone-management@sha256:dfda9128fe36ae2b314dda51fe24163fbcc4bf9961ccfaa9eaa6acf29c75a5d2`;
  and
- fixture client image
  `docker.io/library/busybox@sha256:a46257e06c82ad85242d0d38077d0a52d9896389ca60a1a59d3463db586e5fa4`.

The local image digest identifies only the tested synthetic build. It is not a
published image or a claim that the tag is immutable or externally available.

## Ownership and retention established before apply

The isolated chain was GitRepository -> Kustomization -> HelmRelease. The
Kustomization had pruning disabled and `deletionPolicy: Orphan`; it inventoried
only the HelmRelease. The Helm release inventoried only a ServiceAccount,
ClusterIP Service, and singleton Recreate Deployment. Its ServiceAccount and
Pod did not receive an API token.

Synthetic ConfigMap and Secret inputs and the pre-created runtime claim stayed
outside the chart and all Flux/Helm inventories. The chart neither created nor
deleted a claim. Runtime and evidence PV reclaim policies were changed to and
verified as `Retain` before reconciliation. The proof was not nested below a
prune-enabled parent. Existing retained proof resources were excluded from the
target and were not changed.

The rendered chart, values, Flux objects, controller inventories, expected
normalization, rollback sequence, capacity, collisions, Pod Security, storage
binding, and retained-PV behavior were reviewed before either private Git or
Kubernetes was changed.

## Accepted results

The pinned Git source revision, Kustomization, and HelmRelease converged and
remained Ready. The HelmRelease reported its observed generation and the
revision-derived chart build. The singleton became Ready behind its ClusterIP
Service, and both API and Console checks passed.

After a separately approved replacement of only the isolated synthetic Pod:

- the replacement had a different Pod identity, the same stable coordinator
  hostname, the pinned image digest, zero restarts, and no projected API token;
- exactly one Running Pod referenced the runtime claim;
- Flux remained available and all three reconciliation stages stayed Ready;
- the synthetic runtime marker remained present;
- the previously disabled synthetic credential remained rejected with HTTP
  401; and
- the fixture reported the expected current holder and unchanged epoch.

The reviewed fail-closed probes were separate Pods and did not alter the live
Deployment. A missing required claim remained unschedulable because the claim
did not exist. A missing required Secret prevented its container from starting
because the volume could not mount. Neither input was synthesized and neither
probe container ran.

The sanitized command classes used to verify the result were:

```sh
flux get sources git --all-namespaces
flux get kustomizations --all-namespaces
flux get helmreleases --all-namespaces
kubectl -n <isolated-namespace> get helmrelease,deploy,pod,service,serviceaccount,pvc,job
kubectl get pv
kubectl -n <isolated-namespace> get events --sort-by=.lastTimestamp
kubectl auth can-i --as=system:serviceaccount:<namespace>:<serviceaccount> ...
helm template <isolated-release> charts/wildstone-management --values <reviewed-values>
```

The retained private record contains the exact source revision, rendered and
controller-normalized objects, events, object identities, evidence hashes, and
timestamps. It is not public deployment configuration.

## Deliberately untested and unclaimed

The Owner accepted the install, replacement, continuity, and fail-closed
results while explicitly excluding source revision change/rollback, Flux
suspension, uninstall, deletion, fault injection, and cleanup. Those actions
were not performed. In particular, #317's proposed reversible source-change
and rollback check remains unproven and requires a new exact-action approval.

The minimal fixture exposed a coordinator epoch but did not exercise a stale
writer or receipt-fencing transition. Repository evidence from #287 remains
the applicable receipt and stale-epoch evidence; this exercise does not extend
that claim.

All proof resources and evidence remain retained pending a separate Owner
decision. This same-node synthetic result establishes only isolated Flux
reconciliation, replacement, runtime-data continuity, synthetic credential
revocation continuity, singleton ownership, and fail-closed missing-input
behavior. It does not establish off-host recovery, secret custody, adoption of
an existing release, household migration, new-write-preserving rollback, or
high availability. Those remain under #162, #163, #168, and #180.
