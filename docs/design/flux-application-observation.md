# Scoped Flux application observation

Implements #258's repository status path; #213 reuses it rather than building a
second status API. Repository acceptance does not establish live acceptance.

## Boundaries

The existing Owner-authorized `GET /api/v1/plans/{id}` response gains controller
observations under `deploymentStatus`. Console, CLI and Butler use the same API
and existing authorization; this slice does not grant Agents new access to
Owner-only infrastructure review. Named GETs read only the configured deployment
GitRepository, its Kustomization, the reviewed chart GitRepository/HelmRelease and
Deployment. No list/watch, Secrets, logs, exec, attach, proxy, writes, Helm process
or workload reconciliation is introduced. Broad node/log work stays under #213;
this path returns bounded fixed diagnostic text, never controller messages, raw
errors, URLs, credentials or workload environment values.

The observer is disabled unless privileged local deployment inventory contains
an `observation` section. Existing installations remain compatible. Opening the
manager validates local bootstrap and TLS configuration but installs nothing.
Configuration stays outside the public checkout and local deployment repositories
have no external remotes. Adding/changing observer inventory changes the existing
inventory fingerprint: prepare a fresh Plan after configuration changes.

Synthetic addition to local `deployment.json`:

```json
{
  "observation": {
    "server": "https://127.0.0.1:6443",
    "caFile": "/etc/wildstone-example/observer-ca.pem",
    "tokenFile": "/var/lib/wildstone-example/observer-token",
    "sourceNamespace": "example-flux",
    "sourceName": "example-deployment",
    "kustomizationNamespace": "example-flux",
    "kustomizationName": "example-app",
    "healthURL": "http://example-app.example-app.svc:13378/healthcheck"
  }
}
```

`healthURL` is optional. It is a fixed unauthenticated HTTP `/healthcheck` on a
loopback/private address or local Kubernetes Service name. No proxy, redirect,
query, userinfo or public resolved destination is accepted. The bootstrap owner
must bind it to the isolated application Service, or a dedicated local forwarding
endpoint for that Service when Keystone runs outside cluster DNS. A successful
health response proves that configured endpoint responded; it does not prove
login, library contents or restored user data. Those remain live functional tests.
Without this probe, status can reach `workload_ready`, never application `ready`.

The API uses verified TLS, no proxy and a separate bearer token re-read on each
request for rotation. Keep the token private (0600) outside the checkout. Do not
supply an administrator kubeconfig, the reconciliation identity or a token with
Secret access. The observer cannot determine whether an operator supplied an
overprivileged token; verifying effective RBAC is a bootstrap acceptance gate.

## Read-only RBAC

Create a dedicated ServiceAccount in the isolated application namespace. Bind it
to a Role in each namespace it reads. Grant only `get`, restricted by
`resourceNames`; do not grant cluster-wide list/watch or reuse a reconciler Role.
For the example above, the rules are:

| Namespace | API group / resource | Resource names | Verbs |
| --- | --- | --- | --- |
| example-flux | source.toolkit.fluxcd.io / gitrepositories | example-deployment | get |
| example-flux | kustomize.toolkit.fluxcd.io / kustomizations | example-app | get |
| example-app | source.toolkit.fluxcd.io / gitrepositories | example-app | get |
| example-app | helm.toolkit.fluxcd.io / helmreleases | example-app | get |
| example-app | apps / deployments | example-app | get |

RoleBindings may reference the same ServiceAccount across namespaces. Provision a
rotatable token through established Kubernetes tooling. Before the live proof,
verify these named reads succeed and Secrets, unrelated objects, lists, writes,
logs, exec and proxy are denied. No broad permission is required to observe.

## Meaning of status

Local `phase`, promotion and mirror delivery retain their existing meaning.
Controller reads do not run before a promoted candidate is observed. A configured
observer reports `pending_publication` until that point. `mirrored` remains a
local object comparison, not proof of source-controller reachability.

The observer checks, in order:

1. Deployment source's current Ready generation and exact `deploy@sha1:<SHA>`.
2. Kustomization's configured source/path, current Ready generation and exact
   last-applied revision.
3. HelmRelease's current Ready generation and reviewed spec, including exact
   values, image, namespace/release, chart source reference and pinned chart.
4. Chart GitRepository's reviewed URL, exact commit and current Ready artifact.
5. Deployment's current generation, singleton desired/updated/ready/available
   replica counts, release identity and reviewed image/timezone.
6. Optional bounded local health probe. Before reporting application `ready`,
   re-read all five objects and reject changed resource versions/UIDs.

Missing/forbidden objects or API failure report `dependency_unavailable`; no raw
error is exposed. A current failed/stalled controller reports `failed`. Old
observed generations, prior revisions or incomplete rollout remain `reconciling`;
missing current conditions and changed input are `unknown`. Suspension/deletion
is not readiness. A current Deployment progress-deadline/replica failure is
reported as application failure. A prior healthy revision never satisfies a new
Plan. Fetches are bounded by one five-second observation context; the optional
health request has a two-second limit and 4096-byte maximum body. Read failures
replace readiness, not reuse a cached success.

`controllerObservedAt` is the local collection time, not a controller heartbeat
or transition time. `controllerExpiresAt` expires the UI snapshot after 30 seconds.
Console marks expired controller facts stale and application health unknown even
when refresh requests fail. API consumers must apply the same freshness rule.
A frozen controller reporting the exact desired unchanged generation cannot be
identified as frozen solely from an old Ready transition: this slice makes no
controller-liveness/HA claim.

### Exact Helm input comparison

The live HelmRelease spec must equal the reviewed spec, including **every** key
under values. Extra settings, valuesFrom, alternate chart references, chart
metadata, post-renderers, remote kubeconfig, hooks and unknown future fields are
not silently accepted. A different chart source namespace is also drift even
when image/timezone and Ready generations match.

Only these documented defaults may appear when absent from the reviewed spec:

| Field | Accepted equivalent of omission |
| --- | --- |
| suspend | false |
| targetNamespace | reviewed HelmRelease namespace |
| storageNamespace | reviewed HelmRelease namespace |
| chart.kind | HelmChart |
| chart.spec.sourceRef.namespace | reviewed HelmRelease namespace |
| uninstall | exactly {deletionPropagation: background} |

The namespace defaults and suspend semantics are documented in the
[Helm v2 API](https://fluxcd.io/flux/components/helm/api/v2/); chart kind and source
namespace defaults in the [HelmRelease guide](https://fluxcd.io/flux/components/helm/helmreleases/).
The uninstall deletionPropagation default is also defined in the HelmRelease v2
CRD schema. No defaults are stripped inside values. Explicit reviewed fields
remain mandatory, and unknown/null/empty additions remain a mismatch. Additional
normalization requires a documented semantic default and regression tests.

## Repeat the isolated VM proof

Use the current reviewed source, synthetic data, a dedicated namespace/release,
new PVCs, no external Git remotes and the existing delivery runbooks. Never adopt
or mount the owner-managed release/data. Verify the separate observer identity's
permissions before enabling it. Keep configuration, keys and raw evidence local.

1. Provision the required local identity/OIDC stack for a **real fresh Owner
   Console/API session**. Existing test helpers or direct Manager calls are
   repository evidence and do not satisfy this gate. Do not disable auth.
2. Prepare an allowed harmless timezone change through existing Plans. Before
   approval, verify the serving ref and Flux remain unchanged. Approve through
   the authenticated public API/Console; verify exact promoted, mirrored,
   fetched/applied revisions and current Helm/Deployment generations. Read the
   product API and Console, not just kubectl. Exercise login and synthetic data.
3. Verify the local probe is for this application; unavailable/failed health must
   not become ready. Break observer access temporarily using its separate local
   token, then restore it: API reports dependency unavailable and recovers.
   Let a UI snapshot expire during loss of refresh and check stale labeling.
4. Keep invalid timezone rejection as a pre-publication negative test. The
   closed profile intentionally cannot publish arbitrary broken YAML. Do not
   weaken it or advance refs manually to manufacture a runtime failure.
5. On the **synthetic release only**, select a controlled runtime-dependency
   failure in separately owned bootstrap material (for example withhold the
   named synthetic Secret during a newly approved timezone rollout). Record
   reconciling/failure evidence for the current generation. Restore the missing
   prerequisite, approve the next valid correction through the same API, and
   prove exact revision convergence plus application/data recovery. Be explicit:
   restoring a bootstrap Secret is not recovery performed by Git. This proves
   runtime failure visibility and authorized subsequent convergence; it does not
   prove that an invalid approved YAML configuration was repaired by Git alone.
   If strict invalid-approved-config recovery is still required, record it as an
   unmet gate requiring an explicitly reviewed valid-input failure fixture.
6. Recheck independent delivery with Keystone absent using the isolated setup;
   do not stop unrelated owner services. Preserve existing applications/claims.
7. Remove only newly created synthetic resources, disable temporary services,
   close listeners and record what remains. Relinquish by disabling this local
   observer/publisher and retaining/exporting the local repositories and claims;
   this introduces no uninstall, Flux prune or data deletion. Reassigning Flux
   ownership/prune behavior still requires explicit review.

Post sanitized findings on #258 with tested source SHA, actual API/Console versus
Flux outcomes, failure/recovery and persistence evidence, cleanup and remaining
gates. Do not claim live acceptance from repository fixtures. Parent #254 stays
open until its complete request-to-usable-application path is accepted.

Sources: [Flux Kustomization status](https://fluxcd.io/flux/components/kustomize/kustomizations/),
[HelmRelease status](https://fluxcd.io/flux/components/helm/helmreleases/),
[Helm v2 API](https://fluxcd.io/flux/components/helm/api/v2/).
