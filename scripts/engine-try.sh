#!/usr/bin/env bash
# Try the W050 k3s engine on a THROWAWAY machine (slice 3a, #159).
#
#   sudo scripts/engine-try.sh <public-key-hex> <playbook-dir> <k3s-version>
#
# Verifies the signed playbook set first and runs nothing if it fails;
# then installs a single k3s server beside Docker and prints the facts
# #129 records. Never run it on a machine you care about.
set -euo pipefail

if [[ $# -ne 3 ]]; then
	echo "usage: sudo $0 <public-key-hex> <playbook-dir> <k3s-version>" >&2
	exit 2
fi
pub=$1 dir=$2 version=$3

if [[ $EUID -ne 0 ]]; then
	echo "engine-try: run as root (sudo)" >&2
	exit 1
fi
for tool in wild-playbooks ansible-playbook; do
	if ! command -v "$tool" >/dev/null; then
		echo "engine-try: $tool is not on PATH" >&2
		exit 1
	fi
done

# The signature check comes first. A playbook set that fails it never runs.
wild-playbooks verify -pub "$pub" "$dir"

run() {
	# The signed set's own ansible.cfg, never one from the working
	# directory, the home directory or /etc/ansible.
	ANSIBLE_CONFIG="$dir/ansible.cfg" ANSIBLE_NOCOLOR=1 \
		ansible-playbook -c local -i localhost, "$dir/$1" -e "k3s_version=$version"
}
run k3s-server.yml
run engine-addons.yml

echo
echo "== facts for #129 =="
k3s --version | head -1
k3s kubectl get nodes -o wide
k3s kubectl get namespaces -L pod-security.kubernetes.io/enforce
k3s kubectl get validatingadmissionpolicy,validatingadmissionpolicybinding
k3s kubectl -n wildstone-system get serviceaccounts,configmap/wildstone-authority
# k3s renames its main process k3s-server; count both names. The unit's
# cgroup total also covers containerd and the system pods it started.
echo "k3s process memory (RSS, MiB): $(ps -o rss= -C k3s-server -C k3s | awk '{s+=$1} END {printf "%.0f", s/1024}')"
echo "k3s unit memory (cgroup, MiB): $(systemctl show k3s -p MemoryCurrent --value | awk '$1 ~ /^[0-9]+$/ {printf "%.0f", $1/1048576; next} {print "unknown"}')"
free -m | awk 'NR<=2'
du -sh /var/lib/rancher 2>/dev/null || true
