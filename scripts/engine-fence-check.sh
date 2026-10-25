#!/usr/bin/env bash
# Prove the leader fence on a THROWAWAY k3s machine (W050 slice 3b, #159;
# safety contract #180 §2 and §7). Run it after engine-try.sh:
#
#   sudo scripts/engine-fence-check.sh            # the fence itself
#   sudo scripts/engine-fence-check.sh --rebind   # also the frozen-record issue
#
# It acts as each identity with kubectl impersonation (--as), which the
# admission policy sees exactly as it sees a real token. It adds a second,
# temporary runtime identity to play an old leader on another node, and
# removes it, its test objects and its epoch changes when it finishes.
#
# --rebind also reproduces the frozen-record issue (safety contract §2.6):
# it re-creates the fence's binding, shows the epoch barrier refusing to
# open, then restarts k3s and shows the fence working again.
# Never run it on a machine you care about.
set -euo pipefail

rebind=0
case ${1:-} in
"") ;;
--rebind) rebind=1 ;;
*)
	echo "usage: sudo $0 [--rebind]" >&2
	exit 2
	;;
esac
# How --rebind restarts the API server. Only a test harness overrides it.
restart_api=${FENCE_CHECK_RESTART:-systemctl restart k3s}

if [[ $EUID -ne 0 ]]; then
	echo "fence-check: run as root (sudo)" >&2
	exit 1
fi

k() { k3s kubectl "$@"; }
sa=system:serviceaccount:wildstone-system
node=$(k get nodes -o jsonpath='{.items[0].metadata.name}')
leader=$sa:wardstone-$node
other=$sa:wardstone-fence-test
keystone=$sa:keystone
apps=wildstone-apps
work=$(mktemp -d)

orig_epoch=$(k -n wildstone-system get configmap wildstone-authority -o jsonpath='{.data.epoch}')
orig_leader=$(k -n wildstone-system get configmap wildstone-authority -o jsonpath='{.data.leader}')

cleanup() {
	k -n "$apps" delete configmap -l wildstone.dev/fence-check=1 --ignore-not-found >/dev/null || true
	k -n "$apps" delete rolebinding wardstone-fence-test --ignore-not-found >/dev/null || true
	k -n wildstone-system delete serviceaccount wardstone-fence-test --ignore-not-found >/dev/null || true
	k -n wildstone-system patch configmap wildstone-authority --type=merge \
		-p "{\"data\":{\"epoch\":\"$orig_epoch\",\"leader\":\"$orig_leader\"}}" >/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT

passed=0 failed=0
ok() { echo "PASS  $1"; passed=$((passed + 1)); }
bad() { echo "FAIL  $1"; sed 's/^/      /' "$work/out"; failed=$((failed + 1)); }

# allowed <what> <command...>: the command must succeed.
allowed() {
	local what=$1
	shift
	if "$@" >"$work/out" 2>&1; then ok "$what"; else bad "$what"; fi
}

# refused <what> <pattern> <command...>: the command must fail with output
# matching pattern, so a typo or a missing object can't pass as a refusal.
refused() {
	local what=$1 pattern=$2
	shift 2
	if "$@" >"$work/out" 2>&1; then
		bad "$what (it was allowed)"
	elif grep -qE "$pattern" "$work/out"; then
		ok "$what"
	else
		bad "$what (failed for another reason)"
	fi
}

# can <yes|no> <identity> <verb> <resource> <namespace>: an RBAC check.
can() {
	local want=$1 who=$2 verb=$3 res=$4 ns=$5 got
	got=$(k auth can-i --as="$who" "$verb" "$res" -n "$ns" 2>/dev/null || true)
	echo "$got" >"$work/out"
	if [[ $got == "$want" ]]; then ok "${who##*:} can-i $verb $res -n $ns = $want"; else bad "${who##*:} can-i $verb $res -n $ns = $want"; fi
}

# obj <name> <epoch> [delete-epoch]: a Wildstone-owned ConfigMap in apps.
obj() {
	local extra=""
	[[ -n ${3:-} ]] && extra="    wildstone.dev/delete-epoch: \"$3\""
	cat >"$work/$1.yaml" <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: $1
  namespace: $apps
  labels: {wildstone.dev/fence-check: "1"}
  annotations:
    wildstone.dev/epoch: "$2"
$extra
data: {stamp: "$2-${3:-}"}
EOF
	echo "$work/$1.yaml"
}

as_keystone_set() {
	k --as="$keystone" -n wildstone-system patch configmap wildstone-authority --type=merge \
		-p "{\"data\":{\"epoch\":\"$1\",\"leader\":\"$2\"}}"
}

# barrier <identity> <epoch>: wait up to 10s until this API server admits
# a dry-run write by identity in epoch (contract §2.6). Keystone sends no
# work in a new term until every server's API server passes it.
barrier() {
	for _ in $(seq 1 50); do
		if k --as="$1" create --dry-run=server -f "$(obj fc-barrier "$2")" >"$work/out" 2>&1; then
			return 0
		fi
		sleep 0.2
	done
	return 1
}

# term <epoch> <leader> <what>: Keystone starts a term, then the barrier.
term() {
	if ! as_keystone_set "$1" "$2" >"$work/out" 2>&1; then
		bad "$3: Keystone sets epoch $1"
	elif barrier "$2" "$1"; then
		ok "$3: Keystone sets epoch $1; the barrier opens for ${2##*:}"
	else
		bad "$3: Keystone sets epoch $1, but the barrier never opened for ${2##*:}"
	fi
}

# delete_at <identity> <name> <resourceVersion>: a delete with the stamp's
# resourceVersion as its precondition, as the leader sends it (§2.4, §3).
delete_at() {
	printf '{"kind":"DeleteOptions","apiVersion":"v1","preconditions":{"resourceVersion":"%s"}}' "$3" >"$work/del.json"
	k --as="$1" delete --raw "/api/v1/namespaces/$apps/configmaps/$2" -f "$work/del.json"
}

# stamp <identity> <name> <epoch>: the leader's pre-delete update. Sets rv
# to the stamped object's resourceVersion, or records a failure.
rv=""
stamp() {
	if rv=$(k --as="$1" -n "$apps" annotate configmap "$2" --overwrite \
		"wildstone.dev/epoch=$3" "wildstone.dev/delete-epoch=$3" -o jsonpath='{.metadata.resourceVersion}' 2>"$work/out"); then
		ok "${1##*:} stamps $2 with delete-epoch $3"
	else
		bad "${1##*:} stamps $2 with delete-epoch $3"
		rv=0
	fi
}

echo "== setup: a temporary second runtime identity, playing another node =="
k -n wildstone-system create serviceaccount wardstone-fence-test >/dev/null
k -n "$apps" create rolebinding wardstone-fence-test --role=wardstone-runtime \
	--serviceaccount=wildstone-system:wardstone-fence-test >/dev/null
k -n "$apps" delete configmap -l wildstone.dev/fence-check=1 --ignore-not-found >/dev/null

echo "== RBAC: runtime and Keystone identities are narrow =="
can yes "$leader" create deployments "$apps"
can no "$leader" create rolebindings "$apps"
can no "$leader" create clusterrolebindings default
can no "$leader" patch validatingadmissionpolicies default
can no "$leader" update configmaps/wildstone-authority wildstone-system
can no "$leader" create pods/exec "$apps"
can no "$leader" create pods/attach "$apps"
can no "$leader" list secrets "$apps"
can no "$leader" create configmaps kube-system
can no "$leader" patch namespaces default
can yes "$keystone" update configmaps/wildstone-authority wildstone-system
can no "$keystone" create deployments "$apps"
can yes "$keystone" get leases/wardstone-leader wildstone-system
can no "$keystone" update leases/wardstone-leader wildstone-system

echo "== no leader yet (epoch 0): nothing is written =="
as_keystone_set 0 "" >/dev/null
# No barrier for "no leader"; wait for the record to reach admission.
for _ in $(seq 1 50); do
	k --as="$leader" create --dry-run=server -f "$(obj fc-barrier 0)" >/dev/null 2>&1 || break
	sleep 0.2
done
refused "write with no leader" "denied request" k --as="$leader" create -f "$(obj fc-none 0)"

echo "== term 7: this node leads =="
term 7 "$leader" "this node leads"
allowed "leader creates in epoch 7" k --as="$leader" create -f "$(obj fc-a 7)"
refused "leader write stamped with an older term (6)" "denied request" k --as="$leader" create -f "$(obj fc-b 6)"
refused "leader write with no epoch" "denied request" k --as="$leader" -n "$apps" create configmap fc-c --from-literal=x=1
refused "another node's identity creates" "denied request" k --as="$other" create -f "$(obj fc-d 7)"
refused "another node's identity updates" "denied request" k --as="$other" -n "$apps" annotate configmap fc-a --overwrite wildstone.dev/epoch=7 x=1
refused "another node's identity deletes" "denied request" k --as="$other" -n "$apps" delete configmap fc-a
refused "leader deletes without a stamp" "denied request" k --as="$leader" -n "$apps" delete configmap fc-a
refused "Keystone writes an app object" "cannot create" k --as="$keystone" create -f "$(obj fc-e 7)"
refused "leader changes the authority record" "cannot patch" k --as="$leader" -n wildstone-system patch configmap wildstone-authority --type=merge -p '{"data":{"epoch":"99"}}'

stamp "$leader" fc-a 7
refused "delete with a stale precondition" "Conflict|conflict|precondition" delete_at "$leader" fc-a "$((rv - 1))"
allowed "leader stamps, then deletes at the stamp's version" delete_at "$leader" fc-a "$rv"

echo "== term 8: the other node takes over; this node is now an old leader =="
allowed "leader creates fc-old in epoch 7" k --as="$leader" create -f "$(obj fc-old 7)"
term 8 "$other" "the other node leads"
refused "old leader writes in its own term (7)" "denied request" k --as="$leader" -n "$apps" annotate configmap fc-old --overwrite x=2
refused "old leader writes claiming the new term (8)" "denied request" k --as="$leader" -n "$apps" annotate configmap fc-old --overwrite wildstone.dev/epoch=8
refused "old leader deletes" "denied request" k --as="$leader" -n "$apps" delete configmap fc-old
refused "new leader deletes an epoch-7 object unstamped" "denied request" k --as="$other" -n "$apps" delete configmap fc-old
stamp "$other" fc-old 8
allowed "new leader stamps the epoch-7 object, then deletes it" delete_at "$other" fc-old "$rv"

echo "== term 9: same node again; its term-8 stamp is stale =="
allowed "other node creates fc-same in epoch 8" k --as="$other" create -f "$(obj fc-same 8)"
stamp "$other" fc-same 8
term 9 "$other" "same node, new term"
refused "same node deletes with its term-8 stamp" "denied request" delete_at "$other" fc-same "$rv"
refused "same node writes in term 8" "denied request" k --as="$other" create -f "$(obj fc-f 8)"
allowed "same node writes in term 9" k --as="$other" create -f "$(obj fc-g 9)"

if [[ $rebind -eq 1 ]]; then
	echo "== --rebind: a re-created binding freezes the authority record (§2.6) =="
	term 10 "$leader" "before rebinding"
	k delete validatingadmissionpolicybinding wildstone-leader-fence >/dev/null
	# The freeze needs a moment with no binding, as after a hand delete.
	sleep 3
	k apply --server-side --field-manager=wildstone-bootstrap -f /etc/wildstone/k3s/fence-policy.yaml >/dev/null
	sleep 3
	as_keystone_set 11 "$other" >/dev/null
	if barrier "$other" 11; then
		echo "INFO  not reproduced: this API server picked up epoch 11 after rebinding"
	else
		ok "the barrier stays shut after rebinding, so the frozen record is caught"
	fi
	if k --as="$leader" create --dry-run=server -f "$(obj fc-x 10)" >/dev/null 2>&1; then
		echo "INFO  meanwhile the old leader is still admitted in epoch 10 (the fence is frozen open)"
	else
		echo "INFO  meanwhile the old leader is refused"
	fi
	echo "      restarting the API server: $restart_api"
	$restart_api
	for _ in $(seq 1 90); do
		k get --raw /readyz >/dev/null 2>&1 && break
		sleep 2
	done
	if barrier "$other" 11; then ok "after the restart the barrier opens"; else bad "after the restart the barrier opens"; fi
	refused "after the restart the old leader is refused" "denied request" k --as="$leader" create -f "$(obj fc-y 10)"
fi

echo
echo "== fence check: $passed passed, $failed failed =="
[[ $failed -eq 0 ]]
