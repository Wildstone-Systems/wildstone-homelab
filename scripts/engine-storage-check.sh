#!/usr/bin/env bash
# Prove Longhorn storage on a THROWAWAY k3s machine (W050 slice 3b-3,
# #159; plan D4; safety contract #180 §5). Run it after engine-try.sh:
#
#   sudo scripts/engine-storage-check.sh
#
# In wildstone-apps, under Pod Security "restricted", it shows that:
#   - a wildstone-longhorn claim (ReadWriteOncePod) binds and is healthy;
#   - data written by one pod is read back by the next;
#   - a second pod can't use the claim while the first does (one writer);
#   - deleting the claim keeps the volume (Retain).
# It removes its pods, claim and volume when it finishes. Never run it on
# a machine you care about.
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
	echo "storage-check: run as root (sudo)" >&2
	exit 1
fi

k() { k3s kubectl "$@"; }
ns=wildstone-apps
pvc=wildstone-storage-check
image="docker.io/library/busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"
token="proof-$(date +%s)-$RANDOM"
pv=""

cleanup() {
	k -n "$ns" delete pod -l wildstone.dev/storage-check=1 --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1 || true
	k -n "$ns" delete pvc "$pvc" --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1 || true
	if [[ -n $pv ]]; then
		k delete pv "$pv" --ignore-not-found --timeout=120s >/dev/null 2>&1 || true
		k -n longhorn-system delete volumes.longhorn.io "$pv" --ignore-not-found --timeout=180s >/dev/null 2>&1 || true
	fi
}
trap cleanup EXIT

passed=0 failed=0
ok() { echo "PASS  $1"; passed=$((passed + 1)); }
bad() { echo "FAIL  $1${2:+: $2}"; failed=$((failed + 1)); }

# pod <name> <command>: a restricted pod on the claim. The command goes
# into a YAML double-quoted string, so it must not contain a quote.
pod() {
	k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: $ns
  labels: {wildstone.dev/storage-check: "1"}
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    runAsGroup: 1000
    fsGroup: 1000
    seccompProfile: {type: RuntimeDefault}
  containers:
    - name: c
      image: $image
      command: ["sh", "-c", "$2"]
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities: {drop: [ALL]}
      volumeMounts: [{name: data, mountPath: /data}]
  volumes:
    - name: data
      persistentVolumeClaim: {claimName: $pvc}
EOF
}

echo "== a claim on wildstone-longhorn =="
k -n "$ns" delete pod -l wildstone.dev/storage-check=1 --ignore-not-found --wait=true >/dev/null
k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $pvc
  namespace: $ns
spec:
  accessModes: [ReadWriteOncePod]
  storageClassName: wildstone-longhorn
  resources: {requests: {storage: 1Gi}}
EOF

pod writer "echo $token > /data/proof && sync && sleep 3600"
if k -n "$ns" wait --for=condition=Ready pod/writer --timeout=300s >/dev/null 2>&1; then
	ok "a restricted pod mounts the claim and writes"
else
	bad "a restricted pod mounts the claim and writes" "$(k -n "$ns" describe pod writer | tail -5 | tr '\n' ' ')"
fi
pv=$(k -n "$ns" get pvc "$pvc" -o jsonpath='{.spec.volumeName}')
state=$(k -n longhorn-system get volumes.longhorn.io "$pv" -o jsonpath='{.status.state}/{.status.robustness}' 2>/dev/null || true)
if [[ $state == attached/healthy ]]; then ok "the Longhorn volume is attached and healthy"; else bad "the Longhorn volume is attached and healthy" "got '$state'"; fi
reclaim=$(k get pv "$pv" -o jsonpath='{.spec.persistentVolumeReclaimPolicy}' 2>/dev/null || true)
if [[ $reclaim == Retain ]]; then ok "the volume is Retain"; else bad "the volume is Retain" "got '$reclaim'"; fi

echo "== one writer at a time (ReadWriteOncePod) =="
pod second "sleep 3600"
sleep 20
phase=$(k -n "$ns" get pod second -o jsonpath='{.status.phase}')
if [[ $phase == Pending ]]; then ok "a second pod can't use the claim while the first does"; else bad "a second pod can't use the claim while the first does" "it is $phase"; fi
k -n "$ns" delete pod second --wait=true >/dev/null

echo "== the data outlives the pod =="
k -n "$ns" delete pod writer --wait=true --timeout=120s >/dev/null
pod reader "cat /data/proof"
for _ in $(seq 1 60); do
	phase=$(k -n "$ns" get pod reader -o jsonpath='{.status.phase}')
	[[ $phase == Succeeded || $phase == Failed ]] && break
	sleep 5
done
got=$(k -n "$ns" logs reader 2>/dev/null || true)
if [[ $got == "$token" ]]; then ok "the next pod reads what the first wrote"; else bad "the next pod reads what the first wrote" "read '$got'"; fi

echo "== deleting the claim keeps the data =="
k -n "$ns" delete pod reader --wait=true >/dev/null
k -n "$ns" delete pvc "$pvc" --wait=true --timeout=120s >/dev/null
sleep 5
pvphase=$(k get pv "$pv" -o jsonpath='{.status.phase}' 2>/dev/null || true)
if [[ $pvphase == Released ]]; then ok "the volume is kept (Released) after the claim is gone"; else bad "the volume is kept after the claim is gone" "it is '$pvphase'"; fi
if k -n longhorn-system get volumes.longhorn.io "$pv" >/dev/null 2>&1; then ok "Longhorn still holds the volume"; else bad "Longhorn still holds the volume"; fi

echo
echo "== storage check: $passed passed, $failed failed =="
[[ $failed -eq 0 ]]
