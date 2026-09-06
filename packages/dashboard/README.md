# dashboard

Homepage, family-facing, behind Authelia forward-auth.

The package runs the container. **Everything the dashboard shows is
rendered by Keystone** from desired state —
`keystone/reconcile/homepageservices.go` and `homepagelayout.go` — and
bind-mounted over Homepage's own config:

| File | Contents |
| --- | --- |
| `services.yaml` | One card per Route, grouped, with icon, description and status probe |
| `settings.yaml` | Title, theme, and the per-group grid layout |
| `widgets.yaml` | Info widgets: greeting, clock, host resources |
| `bookmarks.yaml` | Deliberately empty |

All four are node prereqs and all four are in the dashboard container's
config digest, so a change actually reaches the running Homepage (F73).

Why rendered rather than left to the operator: an **unmanaged** Homepage
config file gets Homepage's own sample content seeded into it, which is
how "My First Service" (F55) and "My First Group / Github" (F87) appeared
on healthy installs. And the layout NAMES ITS GROUPS, so a hand-edited
`settings.yaml` would name groups desired state may no longer produce.

Adding a service to the dashboard is documented in
[`../README.md`](../README.md#appearing-on-the-dashboard-f55-f87) — declare
a Route, add one row to `homepageApps`. A missing row now fails
`TestEveryShippedRouteHasAPresentationRow`.

## Not done, and why

Recorded here rather than left in someone's head — silent deferral is not
a state (ADR-W018 A4).

### Icons come from a CDN

`icon: jellyfin` resolves against dashboard-icons over the internet. A
tailnet-only household with no egress — which is a posture this project
deliberately supports (`ingress-tailnet`) — sees names and no logos.

The fix is local icons: vendor the SVGs (they are text), embed them in
the Keystone binary, render them to `<prefix>/dashboard/icons/`, mount
that read-only at `/app/config/icons`, and emit `icon: /icons/<slug>.svg`.
The renderer change is small.

**Blocked on a decision, not on code:** vendoring third-party app logos
into this repository is a licensing and trademark call for the project
owner, not something to do incidentally.

### The status dot means less than it looks like

`siteMonitor` dials the container over the Docker network, so a green dot
means *the application answers*. It does **not** mean the family can
reach it: F83 was Caddy refusing a config it had been handed and serving
stale routing while every container stayed healthy. Green dot, broken
link.

Probing the front door instead (`routeFrontDoor`, as the Route health
gate does) would make the dot mean what a person experiences. **Blocked
on** the trust context ADR-W019 A7 describes: under `tls: internal` the
certificate is issued by the household CA, which the Homepage container
does not trust, so it needs the CA mounted and `NODE_EXTRA_CA_CERTS` set.

### Per-service widgets

Jellyfin now-playing, Immich counts, Uptime Kuma incidents — each needs
that app's API key. Secrets are write-only (ADR-W012 W2) and only a
materializing write resolves a `secret/` reference, so `widgets.yaml`
would need a materialize flag conditional on which apps are installed.
Same shape as F81; needs the secret-fragment decision first.

### Docker integration — recommended AGAINST

Homepage's `docker.yaml` gives per-container stats and start/stop
buttons. It also means handing the Docker socket to a family-facing web
app, which is host root. Not worth it for a stats panel; if it is ever
wanted, only through a read-only socket proxy, and that is an ADR.

### Smaller

- **`custom.css`** — one managed stylesheet for tablet-sized tap targets.
  Nothing blocks it.
- **`family_private` grouping** — currently rendered alongside
  `family_public` under Household. Splitting it is a product call.
- **Port instability (F79, F82)** — services move ports when others are
  installed, so bookmarks and open tabs break. The dashboard's own port
  is pinned, so the practical guidance is "bookmark the dashboard, not
  the service". That belongs in the install docs; it is not a dashboard
  fix.
