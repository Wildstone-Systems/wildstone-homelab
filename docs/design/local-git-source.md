# Read-only local Git delivery for Flux

Status: bounded #257 follow-up built from #264’s head; #264 has now merged, not a completed operating path or authorization to modify the development VM. The opt-in [Owner Plan integration](approved-local-deployment.md) supplies Console/public-API dispatch and automatic Plan resume; its CI/review and the transport proof remain #257 gates. #258 owns actual controller reachability, application observation and the scheduled live proof. ADR-W050 and the migration plan govern.

## Use established services

OpenSSH handles authentication and transport; Git handles upload-pack and local object transfer; systemd supervises the endpoint and a local mirror timer. Keystone implements no Git listener or protocol and is not a dependency of these services. There is no GitHub deployment repository, external push remote, Gitea requirement or backup dependency.

The local layout has three distinct roles:

| Local artifact | Authority / access |
|---|---|
| Canonical product Statefile | Existing product publication; never served to Flux |
| Private deployment promotion repository | #263/#264 journaled publisher; contains prepared candidates and the promoted `deploy` ref |
| Dedicated delivery mirror | Standard Git fetch copies only the promoted branch and its history; OpenSSH serves this object database |

A separate delivery object database is deliberate. Upstream Git documents that hiding refs and disabling arbitrary object wants are not a complete confidentiality boundary for other objects in the same repository. The reader must not reach private prepared candidates. The mirror uses ordinary `file://` upload-pack transfer, with no local-clone hardlinks, shared alternates, configured remotes, tag fetching, submodule recursion or network source. It never fetches prepared refs. It preserves approved history needed for normal Git operation.

The mirror timer requests only `refs/heads/deploy:refs/heads/deploy`, without force. A rewind or conflicting mirror ref edit fails rather than overwriting history. Missing source/ref or failed transfers leave the last mirror ref intact. This additional delivery stage can be stale: promotion, mirror delivery, Flux reconciliation and application readiness must remain distinct observations in #258. Standard systemd/Flux errors are evidence of delivery failure, not application success.

## Render a review bundle

From the Keystone module, `go run ./cmd/local-git-source -config /absolute/local/source.json -out /absolute/local/new-review-directory` renders eight non-secret files. Use an installed binary for deployment; source-tree execution here describes the developer entry point only. The JSON input must be a private regular file outside every source checkout; the output must be new and outside source/runtime directories. Rendering validates the initialized private promotion repository, but creates no repository, account, key, listener, service or Kubernetes object.

Synthetic loopback example (not pod-reachable deployment settings):

```json
{
  "listenAddress": "127.0.0.1",
  "port": 22222,
  "clientCIDR": "127.0.0.1/32",
  "serviceUser": "example-source",
  "repository": "/var/lib/wildstone-example/deployment.git",
  "mirrorRepository": "/var/lib/wildstone-example/delivery.git",
  "installDirectory": "/etc/wildstone-example-source",
  "namespace": "example-app",
  "sourceName": "example-local",
  "credentialSecret": "example-source-key"
}
```

Choose the installation's specific private node address and the actual minimum source-controller egress CIDR locally. Wildcard/public listeners, hostnames, public/broad client networks, root/nobody accounts, nested repositories/bootstrap keys and shell-interpolated paths are refused. A loopback listener alone cannot be reached from ordinary cluster pods. No router, DNS, Tailscale or public networking change is implied.

The generated bundle contains `sshd_config`, `upload-pack`, `wildstone-git-source.service`, `init-mirror`, `mirror`, `wildstone-git-mirror.service`, `wildstone-git-mirror.timer` and `gitrepository.yaml`. Files are private review material, not artifacts to commit to the public repository. Rendering refuses a foreign or externally configured promotion repository and never initializes a missing one.

## Bootstrap contract, not an executed installation

A separately reviewed bootstrap must establish the following before enabling anything:

1. Provide supported Git, OpenSSH server/client and systemd, including the distribution's normal SSH privilege-separation runtime directory. Preserve existing host SSH configuration and listeners; the generated daemon uses its own `-f` configuration and dedicated port.
2. Select an unprivileged repository-owning runtime account with a usable `/bin/sh` login shell, locked password and root-controlled session startup configuration. Its access must permit the mirror job to read the promotion repository and write its own private mirror. Preserve the promotion repository's 0700 boundary; do not solve access by making it generally readable or changing ownership of working deployments. The source key is accepted only by this dedicated listener's root-controlled authorized-keys file.
3. Install configuration/wrappers under the selected root-controlled install directory. The account needs traversal/read access to the command scripts, but must not be able to modify scripts, daemon configuration, authorized keys or host keys. For example, use a root-owned 0750 directory and runtime-account group, 0750 scripts, 0640 daemon config and authorized-keys file owned by root with the runtime-account group, and a root-only 0600 host private key. The account must be able to read its authorized-key file while being unable to modify it. Verify all ancestor permissions. Units are installed root-owned in the normal systemd unit directory.
4. Generate separate OpenSSH host and source-controller key pairs locally using established tools. Install only the source-controller public key in this endpoint's authorized-keys file, preferably with `restrict`. Keep private keys out of both Git object databases and the public checkout. Pin the host public key through an independently verified fingerprint; do not accept an unverified network key scan as trust establishment.
5. Run the reviewed `init-mirror` script once as the selected account. It refuses an existing path rather than adopting it. Run the local mirror job once; an empty/unpromoted source honestly fails until an approved ref exists. Enable the independent timer and SSH unit only after account, file, network and `sshd -t` validation succeeds.
6. Provision the referenced namespace-scoped Flux Secret through the existing privileged bootstrap/secret mechanism. Flux requires `identity` and `known_hosts`; the rendered GitRepository contains their Secret reference, never their values. Source credentials carry no Git writer, API approval, agent, node-admin or cluster-admin authority.
7. Review the ordinary application Kustomization, its scoped ServiceAccount/RBAC, source namespace, retention/pruning settings and network policy separately. The generated GitRepository does not create privileged RBAC, a Kustomization or workload ownership and must not bootstrap the endpoint required to read it.

The endpoint permits only the exact SSH command `git-upload-pack '/deployment.git'`. It ignores neither an arbitrary repository argument nor command suffix: other requests fail. The wrapper clears inherited Git environment/config overrides before executing the fixed Git binary against the delivery mirror. The SSH daemon disables forwarding, TTY, user RC/environment, password and keyboard-interactive authentication. Prepared/unrelated refs are not advertised; arbitrary object wants are disabled as defense in depth, not as the object-isolation boundary.

## Evidence and remaining gates

Unit/protocol tests use real temporary bare repositories and standard Git pack transfer. They verify approved bytes can be served, prepared objects do not exist in the serving mirror, known hidden object-ID requests fail, similar ref names are hidden, inherited Git overrides are dropped, unsafe commands/configuration are refused, and generation preserves runtime paths. Statefile inspection tests ensure that validation never creates/repairs a missing or foreign repository. No real deployment data or key fixtures are used.

A real temporary OpenSSH/Git test generates keys at runtime and tests clone/read, wrong client and host keys, source address restriction, push/shell/other-repository refusal and forwarding denial. It uses an existing test identity, not a created host account; a root test fixture substitutes only that identity while production configuration forbids root. It skips when required binaries or mandatory SSH privilege-separation prerequisites are unavailable. The hosted coding runtime lacks root chroot/setuid/setgid capabilities, so its SSH authentication test cannot be counted as executed transport proof. No privilege-separation restriction is disabled to make it pass. This limitation must remain visible until a supported test environment executes it.

Systemd lifecycle, real runtime-account filesystem boundaries and pod-to-endpoint retrieval still need the scheduled #258 proof. Do not mark #257 transport acceptance complete merely because templates render or a test skipped. Do not infer an available endpoint or healthy app from a successful local promotion. No existing release is adopted; no Docker/custom path, identity component or backup is retired.

Sources: [Flux GitRepository SSH authentication](https://fluxcd.io/flux/components/source/gitrepositories/#ssh-authentication), [OpenSSH forced-command and forwarding controls](https://man.openbsd.org/sshd_config), [Git hidden-ref and object-access limitations](https://git-scm.com/docs/git-config#Documentation/git-config.txt-transferhideRefs).
