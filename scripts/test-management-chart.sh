#!/usr/bin/env bash
set -euo pipefail

helm_bin="${HELM:-helm}"
chart="${1:-charts/wildstone-management}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { echo "management chart: $*" >&2; exit 1; }
refute() {
  local pattern="$1" file="$2" message="$3"
  if grep -Eq "$pattern" "$file"; then
    fail "$message"
  fi
}

"$helm_bin" lint "$chart"
"$helm_bin" template synthetic "$chart" > "$work/default-a.yaml"
"$helm_bin" template synthetic "$chart" > "$work/default-b.yaml"
cmp "$work/default-a.yaml" "$work/default-b.yaml"
rendered="$work/default-a.yaml"

[ "$(grep -c '^kind: Deployment$' "$rendered")" -eq 1 ] || fail "expected one Deployment"
[ "$(grep -c '^kind: ServiceAccount$' "$rendered")" -eq 1 ] || fail "expected one ServiceAccount"
grep -Eq '^  replicas: 1$' "$rendered" || fail "singleton replica count is absent"
grep -A1 -E '^  strategy:$' "$rendered" | grep -q 'type: Recreate' || fail "Recreate strategy is absent"
grep -q 'image: ".*@sha256:[a-f0-9]\{64\}"' "$rendered" || fail "image is not digest-pinned"
[ "$(grep -c 'automountServiceAccountToken: false' "$rendered")" -eq 2 ] || fail "ServiceAccount token mounting is not disabled on both objects"
grep -A2 'name: runtime$' "$rendered" | grep -q 'persistentVolumeClaim:' || fail "runtime is not a PVC"
grep -q 'claimName: wildstone-management-runtime' "$rendered" || fail "required runtime claim reference is absent"
grep -q '^        runAsUser: 65532$' "$rendered" || fail "Pod does not use the fixed unprivileged uid"
grep -q '^        runAsGroup: 65532$' "$rendered" || fail "Pod does not use the fixed unprivileged primary gid"
grep -q '^        runAsNonRoot: true$' "$rendered" || fail "Pod does not require a non-root process"
grep -q '^        fsGroup: 65532$' "$rendered" || fail "Pod does not assign the storage group"
grep -q '^        fsGroupChangePolicy: OnRootMismatch$' "$rendered" || fail "Pod does not use bounded fsGroup ownership reconciliation"
grep -A1 'seccompProfile:' "$rendered" | grep -q 'type: RuntimeDefault' || fail "RuntimeDefault seccomp is absent"
grep -q '^            readOnlyRootFilesystem: true$' "$rendered" || fail "container root filesystem is writable"
grep -q '^              drop: \["ALL"\]$' "$rendered" || fail "container does not drop every capability"
grep -A3 '^        - name: tmp$' "$rendered" | grep -q 'emptyDir:' || fail "/tmp is not backed by emptyDir"
grep -q 'sizeLimit: 128Mi' "$rendered" || fail "/tmp emptyDir is not bounded"

refute '^kind: (Ingress|ClusterRole|ClusterRoleBinding|Role|RoleBinding|PersistentVolumeClaim)$' "$rendered" "forbidden/public/claim-owning resource rendered"
refute 'initContainers:' "$rendered" "root-dependent initialization rendered"
refute 'runAsUser: *0' "$rendered" "root user rendered"
refute 'runAsNonRoot: *false' "$rendered" "non-root enforcement disabled"
refute 'hostPath:' "$rendered" "hostPath rendered"
refute 'hostNetwork: *true' "$rendered" "host networking rendered"
refute 'privileged: *true' "$rendered" "privileged container rendered"
refute 'allowPrivilegeEscalation: *true' "$rendered" "privilege escalation rendered"
refute 'automountServiceAccountToken: *true' "$rendered" "Kubernetes credentials rendered"
refute 'add: *\[' "$rendered" "added Linux capability rendered"
refute 'type: (NodePort|LoadBalancer)' "$rendered" "public Service rendered"

if "$helm_bin" template synthetic "$chart" --set replicaCount=2 > /dev/null 2>&1; then
  fail "replicaCount=2 passed schema validation"
fi
if "$helm_bin" template synthetic "$chart" --set-string storage.runtime.existingClaim= > /dev/null 2>&1; then
  fail "an empty required runtime claim rendered"
fi
if "$helm_bin" template synthetic "$chart" --set storage.sharedUploads.enabled=true > /dev/null 2>&1; then
  fail "shared uploads silently fell back without a claim"
fi
if "$helm_bin" template synthetic "$chart" --set storage.privateWorkspaces.enabled=true > /dev/null 2>&1; then
  fail "private workspaces silently fell back without a claim"
fi
if "$helm_bin" template synthetic "$chart" --set image.digest=latest > /dev/null 2>&1; then
  fail "a mutable/non-digest image reference passed schema validation"
fi
if "$helm_bin" template synthetic "$chart" --set storage.runtime.preparePermissions=true > /dev/null 2>&1; then
  fail "removed root permission-initialization option still passed schema validation"
fi

"$helm_bin" template synthetic "$chart" \
  --set-string configuration.existingConfigMap=synthetic-config \
  --set-string configuration.existingSecret=synthetic-files \
  --set-string secretEnv[0].name=OPENAI_API_KEY \
  --set-string secretEnv[0].secretName=synthetic-provider \
  --set-string secretEnv[0].key=api-key \
  --set storage.sharedUploads.enabled=true \
  --set-string storage.sharedUploads.existingClaim=synthetic-uploads \
  --set storage.privateWorkspaces.enabled=true \
  --set-string storage.privateWorkspaces.existingClaim=synthetic-workspaces \
  > "$work/references.yaml"
for expected in synthetic-config synthetic-files synthetic-provider synthetic-uploads synthetic-workspaces; do
  grep -q "$expected" "$work/references.yaml" || fail "reference $expected did not render"
done
refute '^kind: Secret$' "$work/references.yaml" "chart embedded a Secret"
refute 'stringData:|^data:' "$work/references.yaml" "chart embedded secret values"

echo "PASS: management chart lint, schema, deterministic render, references, storage and Restricted security checks"

