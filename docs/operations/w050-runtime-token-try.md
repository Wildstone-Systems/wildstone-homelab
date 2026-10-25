# Trying the W050 runtime tokens on a disposable server

This is the first slice 3c check. It mints scoped TokenRequest credentials
for the node's Wardstone and, when requested, Keystone. The root-only timer
renews each token before it expires. Neither daemon receives the k3s admin
kubeconfig. Leadership is not started by this playbook.

Use the same disposable VM as the [engine test](w050-engine-try.md). Start
from a clean repository checkout and a **fresh signed copy** of
`deploy/ansible` (the engine guide's steps 2–3). Never put a token, signing
key or kubeconfig in the repository or in an issue comment.

1. Read this host's Kubernetes node name with
   `sudo k3s kubectl get nodes -o name`. Pass that exact name as
   `wildstone_node_id`; do not infer it from the OS hostname. On the
   single-node test VM, set `wildstone_keystone_here` to true.
2. Syntax-check and run the signed playbook:

   ```sh
   wild-playbooks verify -pub "$PUB" "$PLAYBOOK_DIR"
   ANSIBLE_CONFIG="$PLAYBOOK_DIR/ansible.cfg" ansible-playbook \
     --syntax-check -c local -i localhost, \
     "$PLAYBOOK_DIR/engine-runtime-tokens.yml" \
     -e '{"wildstone_node_id":"node-a","wildstone_keystone_here":true}'
   sudo ANSIBLE_CONFIG="$PLAYBOOK_DIR/ansible.cfg" ansible-playbook \
     -c local -i localhost, "$PLAYBOOK_DIR/engine-runtime-tokens.yml" \
     -e '{"wildstone_node_id":"node-a","wildstone_keystone_here":true}'
   ```

   Replace `node-a` with the node name from step 1. The syntax check and
   signature verification run before the playbook changes the host.
3. Run the same playbook a second time. It should report `changed=0`.
4. As root, inspect **only metadata**, never token contents:

   ```sh
   sudo stat -c '%n %a %U:%G' /etc/wildstone/k3s/{wardstone-node-a,keystone}.token
   sudo systemctl status 'wildstone-runtime-token@wardstone-node-a.timer' \
     'wildstone-runtime-token@keystone.timer' --no-pager
   ```

   Both token files must be `600 root:root`; their directory must be
   `700 root:root`. Both timers must be active. The script prints only
   `rotated`, `repaired` or `current`, never a token.
5. Confirm the credentials authenticate and remain scoped. A Wardstone
   token can read `wildstone-authority` and the leader Lease. A Keystone
   token can read the Lease and update only `wildstone-authority`. Neither
   can list ServiceAccounts, change RBAC or admission, or list Secrets.
   Use a root-only temporary kubeconfig or a local program that reads the
   token file; never pass the token on a command line or paste it into a
   report.

Report the commit, Ansible and k3s versions, syntax and signature results,
both playbook recaps, file modes, timer status and scoped API results to
[#159](https://github.com/Wildstone-Systems/wildstone-homelab/issues/159).
