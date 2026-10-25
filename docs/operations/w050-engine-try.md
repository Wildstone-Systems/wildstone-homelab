# Trying the k3s engine on a throwaway machine (W050 slices 3a and 3b)

This installs a single k3s server, using Wildstone's signed playbooks and reviewed flags, on a machine you can delete afterwards. It's the first hands-on check for [#159](https://github.com/Wildstone-Systems/wildstone-homelab/issues/159) and feeds [#129](https://github.com/Wildstone-Systems/wildstone-homelab/issues/129).

**Never run this on your production machine or your Proxmox host.** Use a new VM.

## What it does and doesn't do

- **Does:**
  - checks the playbook set's signature first, and runs nothing if it fails;
  - installs host prerequisites and a pinned k3s binary, checked against that release's own checksum file;
  - starts k3s with: secrets encryption, kernel-default protection, Pod Security defaults, traefik/servicelb/local-storage off, and **timed force-detach off** (safety contract #180 §5);
  - creates the `wildstone-system`, `wildstone-apps` (restricted) and `wildstone-host` namespaces;
  - creates the **leader fence** (safety contract #180 §2): the authority record (`wildstone-authority`, starting at epoch 0, so nothing is allowed to write yet), the `wildstone-leader-fence` admission policy, a `wardstone-<node>` runtime account per node and a narrow `keystone` account.
- **Doesn't (yet):** run Wardstone's leader election (the code is in slice 3b-2; it's wired into Wardstone in 3c), Longhorn and kube-vip (3b-3), or any app. Keystone and Docker are untouched.

## You need

- A new VM: Ubuntu 24.04 or 26.04, or Debian 12, x86_64 or arm64, 2 vCPU, 4 GB RAM, 20 GB disk, internet access. (First tested on Ubuntu 26.04 with k3s v1.37.1+k3s1; see #159.)
- On the VM: `git`, `ansible-core` (`sudo apt install -y git ansible-core`), and Go 1.26 to build one small tool. You can also build the tool elsewhere and copy it over.

## Steps

1. **Get the code** (on the VM):

   ```sh
   git clone https://github.com/Wildstone-Systems/wildstone-homelab.git
   cd wildstone-homelab
   ```

2. **Build the signing tool:**

   ```sh
   (cd wardstone && go build -o /tmp/wild-playbooks ./cmd/wild-playbooks)
   sudo install -m 0755 /tmp/wild-playbooks /usr/local/bin/wild-playbooks
   ```

3. **Sign a copy of the playbooks with a throwaway key.** A release will be signed in CI; for this test you sign it yourself.

   ```sh
   cp -r deploy/ansible /tmp/playbooks
   PUB=$(wild-playbooks keygen -out /tmp/playbooks-test.key)
   wild-playbooks sign -key /tmp/playbooks-test.key /tmp/playbooks
   echo "$PUB"     # the public key; the script needs it
   ```

4. **Pick a k3s version.** Use the current stable release from the [k3s releases page](https://github.com/k3s-io/k3s/releases). It must be Kubernetes **1.30 or later**, for example `v1.33.5+k3s1`.

5. **Run it:**

   ```sh
   sudo scripts/engine-try.sh "$PUB" /tmp/playbooks v1.33.5+k3s1
   ```

   It ends with a "facts for #129" block: the k3s version, node status, namespaces, the fence objects, k3s memory, free RAM and disk used.

6. **Check the leader fence:**

   ```sh
   sudo scripts/engine-fence-check.sh
   sudo scripts/engine-fence-check.sh --rebind
   ```

   It acts as each identity with `kubectl --as` and plays three leadership terms, including an old leader on another node. It cleans up after itself and puts the epoch back. Every line should say `PASS`, and the last line should read `0 failed`.

   `--rebind` also reproduces a known problem (contract §2.6). After the fence's binding is deleted and re-created, the API server keeps using a frozen copy of the authority record. It should print `PASS  the barrier stays shut` and `INFO  ... the fence is frozen open`, then **restart k3s** and show the fence working again. Report that whole section.

## Checks to run afterwards

| Check | Command | Expect |
|---|---|---|
| Node is ready | `sudo k3s kubectl get nodes` | `Ready` |
| Secrets are encrypted | `sudo k3s secrets-encrypt status` | `Encryption Status: Enabled` |
| Force-detach timer is off | `sudo journalctl -u k3s \| grep -m1 disable-force-detach-on-timeout` | the flag appears in the controller-manager arguments |
| Apps are held to restricted | apply the privileged pod below | refused by Pod Security (`violates PodSecurity "restricted"`) |
| The cluster-wide default applies | apply the same pod with `namespace: default` | refused by Pod Security (`violates PodSecurity "baseline"`): the admission config file was loaded |
| Re-run changes nothing | run step 5 again | Ansible recap shows `changed=0` |
| A tampered set never runs | `echo "# x" >> /tmp/playbooks/k3s-server.yml`, then step 5 | `a listed file is missing or changed`, and nothing runs |
| An extra file never runs | `echo "- hosts: all" > /tmp/playbooks/extra.yml`, then step 5 | `a file is not in the signed manifest` |
| The leader fence holds | step 6, `engine-fence-check.sh` | `42 passed, 0 failed` |
| The frozen record is caught | step 6, `--rebind` | `46 passed, 0 failed`, with `the barrier stays shut` and `frozen open` |
| The epoch is never reset | step 5 again, then `sudo k3s kubectl -n wildstone-system get cm wildstone-authority -o jsonpath='{.data}'` | the recap shows `changed=0`, and the record is unchanged |

The privileged pod for the restricted check:

```sh
cat <<'POD' | sudo k3s kubectl apply -f -
apiVersion: v1
kind: Pod
metadata: {name: bad, namespace: wildstone-apps}
spec: {containers: [{name: c, image: busybox, securityContext: {privileged: true}}]}
POD
```

If k3s fails to start, run `sudo journalctl -u k3s -n 50`. An "unknown flag" there for `disable-force-detach-on-timeout` means the version is older than Kubernetes 1.30.

## Report back

Paste these into #159:
- the "facts for #129" block;
- the results of the checks table;
- the full output of `engine-fence-check.sh --rebind`;
- anything that failed, with `journalctl -u k3s -n 50`.

## Removing it

Delete the VM. That's the clean way. If you must keep the VM:

```sh
sudo systemctl disable --now k3s
sudo rm -rf /usr/local/bin/k3s /etc/systemd/system/k3s.service /etc/rancher/k3s /etc/wildstone/k3s /var/lib/wildstone/k3s /var/lib/rancher
sudo reboot   # clears k3s's network rules and mounts
```
