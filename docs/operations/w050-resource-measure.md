# W050 idle resource comparison

Use `scripts/engine-resource-sample.py` for the #159 measurements that feed
the #129 single-node decision. It reads host counters and fixed data paths;
it does not change services or use API credentials.

Collect two samples on VMs with the same CPU count, memory, disk size and OS.
Use the same Wildstone package state on both. Let installation and image
pulls finish, leave each VM otherwise idle for ten minutes, and avoid other
tests during the sample. The Docker-only baseline must have no k3s binary.

```sh
sudo python3 scripts/engine-resource-sample.py --label docker-baseline > docker-idle.json
sudo python3 scripts/engine-resource-sample.py --label engine-idle > engine-idle.json
```

The default sample lasts two minutes, with ten memory observations and one
host-wide CPU calculation over the entire interval. The JSON records median
available/used RAM, root filesystem use before and after, allocated sizes of the
Docker, k3s and Longhorn data directories, service cgroup memory and
installed versions. A service cgroup is diagnostic only: Docker containers
may live outside `docker.service`, so compare **host-wide** CPU and RAM for
the baseline. The directory sizes can overlap with root filesystem use and
must not be added to it.

Post both redacted JSON records to #159 and #129, along with the VM size,
installed package state and whether kube-vip is active. The records contain
no credential fields, but review them before posting. One engine sample on
the existing test VM is useful now; the Docker comparison needs a separate
fresh baseline VM.
