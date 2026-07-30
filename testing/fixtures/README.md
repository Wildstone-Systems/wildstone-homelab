# Fixture library (E0.3)

Golden configs transcribed from the reference deployment
(`chickenrat17/homelab-config`, branch
`claude/wildstone-homelab-architecture-8owgkg`, 2026-07-30). These are the
test oracle for the capability engine and reconciler: a package that claims
to produce "Authelia protecting an app behind forward-auth" should converge
to configs shaped like these.

## Contents

| Directory | Source | What it demonstrates |
|---|---|---|
| `authelia/` | `deployments/infra-services/authelia/` | Session cookies across two domains; deny-by-default access control with per-group two-factor rules |
| `caddy/` | `deployments/infra-services/caddy/` | Dual-ingress pattern: public domain via Cloudflare Tunnel + Tailscale-only vhosts with `tls internal`; forward-auth to Authelia; path-based multiplexing of one public hostname across several backends |
| `lldap/` | `deployments/infra-services/lldap/` | LLDAP with file-sourced secrets and custom base DN |
| `immich/` | `deployments/media-services/immich/` | Immich OIDC wiring against Authelia (env side transcribed; `authelia-oidc-client.yml` is RECONSTRUCTED — see its header) |
| `jellyfin/` | `deployments/media-services/jellyfin/` | Jellyfin LDAP-Auth plugin against LLDAP over Tailscale; note the `{username}` (not `{0}`) search-filter placeholder and the exact `LDAP-Auth.xml` filename requirement |
| `gluetun/` | `deployments/media-services/gluetun/` | VPN egress container: firewall input ports for the *arr stack, LAN outbound subnet carve-out, DoT, health target |

## Placeholder mapping (scrub rule)

Every fixture uses placeholder values only. CI runs gitleaks on every push;
a fixture that trips it is a bug even if the value is "only" semi-secret.

| Real value (reference deployment) | Placeholder |
|---|---|
| Public domain | `example.com` |
| Tailscale MagicDNS suffix | `tailnet` (kept — already generic in source) |
| LAN subnet `192.168.x.0/24` | `192.0.2.0/24` (TEST-NET-1; host octets preserved so infra-vm vs media-vm stay distinguishable) |
| Tailscale IP of infra-vm | `100.64.0.4` (CGNAT range) |
| LDAP base DN | `dc=example,dc=com` |
| Timezone | `Etc/UTC` |
| Secrets, tokens, account numbers | `REPLACED-*` (e.g. `REPLACED-SESSION-SECRET`, `REPLACED-CLIENT-SECRET`, `REPLACED-MULLVAD-ACCOUNT`, `REPLACED-BIND-PASSWORD`) |

Structure, ports, service names, group names, and quirks are preserved
verbatim — including oddities that are genuinely present in the working
deployment (e.g. gluetun lists `SERVER_COUNTRIES=USA` alongside European
`SERVER_CITIES`; Caddy public vhosts are `http://` because TLS terminates
at the Cloudflare Tunnel). Do not "fix" these; they are what the oracle
actually runs.

## Still to come (E12)

Failure fixtures for Butler diagnostics: broken OIDC secret, dead database,
full disk, detached NFS, and friends — built alongside these happy paths.
