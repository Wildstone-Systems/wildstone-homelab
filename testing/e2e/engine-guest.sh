#!/usr/bin/env bash
# Runs only inside a disposable QEMU guest created by TestEngine.
set -euo pipefail

cd /tmp/wildstone-src
sudo apt-get update -qq
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ansible-core
sudo install -m 0755 /tmp/wild-playbooks /usr/local/bin/wild-playbooks
sudo cp -a deploy/ansible /tmp/playbooks
pub=$(sudo wild-playbooks keygen -out /tmp/playbooks-test.key)
sudo chmod 0600 /tmp/playbooks-test.key
sudo wild-playbooks sign -key /tmp/playbooks-test.key /tmp/playbooks
sudo wild-playbooks verify -pub "$pub" /tmp/playbooks

run_engine() {
  local log=$1
  if ! sudo timeout 45m scripts/engine-try.sh "$pub" /tmp/playbooks v1.37.1+k3s1 >"$log" 2>&1; then
    tail -n 100 "$log"
    return 1
  fi
  grep -E '^localhost[[:space:]]*:' "$log"
  if grep -Eq 'failed=[1-9]|unreachable=[1-9]' "$log"; then
    return 1
  fi
}

run_engine /tmp/engine-first.log
run_engine /tmp/engine-second.log
if grep -Eq 'changed=[1-9]' /tmp/engine-second.log; then
  echo 'repeat engine install changed resources' >&2
  exit 1
fi

sudo k3s kubectl wait --for=condition=Ready node --all --timeout=120s
sudo k3s kubectl -n longhorn-system rollout status daemonset/longhorn-manager --timeout=600s
sudo k3s kubectl -n longhorn-system rollout status deployment/longhorn-driver-deployer --timeout=600s
sudo k3s kubectl get csidriver driver.longhorn.io >/dev/null
sudo k3s secrets-encrypt status | grep -q 'Encryption Status: Enabled'

sudo scripts/engine-storage-check.sh | tail -n 12
sudo scripts/engine-fence-check.sh | tail -n 8
sudo scripts/engine-fence-check.sh --rebind | tail -n 12

# Both signature refusals must happen before any Ansible task is run.
sudo cp -a /tmp/playbooks /tmp/tampered-playbooks
echo '# tampered' | sudo tee -a /tmp/tampered-playbooks/k3s-server.yml >/dev/null
if sudo scripts/engine-try.sh "$pub" /tmp/tampered-playbooks v1.37.1+k3s1 > /tmp/tamper.log 2>&1; then
  echo 'tampered signed playbook was accepted' >&2
  exit 1
fi
grep -q 'a listed file is missing or changed' /tmp/tamper.log
if grep -q 'PLAY RECAP' /tmp/tamper.log; then
  echo 'Ansible ran after a failed signature check' >&2
  exit 1
fi

echo 'QEMU engine bootstrap, repeatability, storage, fence, and tamper checks passed'
