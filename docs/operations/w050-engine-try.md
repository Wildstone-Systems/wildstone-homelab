# Trying the k3s engine on a throwaway machine (W050 slice 3a)

This installs a single k3s server, using Wildstone's signed playbooks and reviewed flags, on a machine you can delete afterwards. It's the first hands-on check for [#159](https://github.com/Wildstone-Systems/wildstone-homelab/issues/159) and feeds [#129](https://github.com/Wildstone-Systems/wildstone-homelab/issues/129).

**Never run this on your production machine or your Proxmox host.** Use a new VM.

## What it does and doesn't do

- **Does:**
  - checks the playbook set's signature first, and runs nothing if it fails;
  - installs host prerequisites and a pinned k3s binary, checked against that release's own checksum file;
  - starts k3s with: secrets encryption, kernel-default protection, Pod Security defaults, traefik/servicelb/local-storage off, and **timed force-detach off** (safety contract #180 §5);
  - creates the `wildstone-system`, `wildstone-apps` (restricted) and `wildstone-host` namespaces.
- **Doesn't (yet, slice 3b):** Longhorn, kube-vip, runtime ServiceAccounts, the fencing admission policy, Wardstone leader election, or any app. Keystone and Docker are untouched.

## You need

- A new VM: Ubuntu 24.04 or Debian 12, x86_64 or arm64, 2 vCPU, 4 GB RAM, 20 GB disk, internet access.
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

   It ends with a "facts for #129" block: the k3s version, node status, namespaces, k3s memory, free RAM and disk used.

## Checks to run afterwards

| Check | Command | Expect |
|---|---|---|
| Node is ready | `sudo k3s kubectl get nodes` | `Ready` |
| Secrets are encrypted | `sudo k3s secrets-encrypt status` | `Encryption Status: Enabled` |
| Force-detach timer is off | `sudo journalctl -u k3s \| grep -m1 disable-force-detach-on-timeout` | the flag appears in the controller-manager arguments |
| Apps are held to restricted | apply the privileged pod below | refused by Pod Security (`violates PodSecurity "restricted"`) |
| Re-run changes nothing | run step 5 again | Ansible recap shows `changed=0` |
| A tampered set never runs | `echo "# x" >> /tmp/playbooks/k3s-server.yml`, then step 5 | `a listed file is missing or changed`, and nothing runs |
| An extra file never runs | `echo "- hosts: all" > /tmp/playbooks/extra.yml`, then step 5 | `a file is not in the signed manifest` |

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
- anything that failed, with `journalctl -u k3s -n 50`.

## Removing it

Delete the VM. That's the clean way. If you must keep the VM:

```sh
sudo systemctl disable --now k3s
sudo rm -rf /usr/local/bin/k3s /etc/systemd/system/k3s.service /etc/rancher/k3s /etc/wildstone/k3s /var/lib/wildstone/k3s /var/lib/rancher
sudo reboot   # clears k3s's network rules and mounts
```
