import { Observed, PackageView, ResourceView } from "./api";
import { Kind } from "./ui/status";

// The service model Home and Services are drawn from.
//
// A GROUP is an owner — `capability/<name>` in the statefile, which is
// how a household thinks of a service ("identity", "jellyfin"). A TILE
// is one container inside it, because that is the thing that is up or
// down and the thing an address points at.
//
// All of it is joined client-side from three calls the console already
// makes. No endpoint was added for this screen (ADR-W027 A1).

export interface Service {
  key: string;
  kind: string;
  name: string;
  node?: string;
  health: Kind;
  state: string;
  detail?: string;
  // Where it answers, if a Route points at it. Absent means the
  // container is reachable only from inside the household network,
  // which is a fact about it and not a gap in our knowledge.
  url?: string;
  urlHealthy?: boolean;
}

export interface ServiceGroup {
  owner: string;
  label: string;
  services: Service[];
  resourceCount: number;
  health: Kind;
  blockers: string[];
  version?: string;
  stale?: boolean;
  catalogVersion?: string;
}

const RANK: Record<Kind, number> = { bad: 0, warn: 1, mute: 2, ok: 3 };

export function ownerLabel(owner: string): string {
  return owner.startsWith("capability/") ? owner.slice("capability/".length) : owner;
}

export function healthOf(o: Observed | undefined): { health: Kind; state: string; detail?: string } {
  if (!o) return { health: "mute", state: "unobserved" };
  // Three states, not two (F75). Held back by a plan waiting for a
  // human is amber; it is not a fault and must not read as one.
  if (o.healthy) return { health: "ok", state: "healthy", detail: o.detail };
  if (o.awaitingApproval) return { health: "warn", state: "waiting on a plan", detail: o.detail };
  return { health: "bad", state: o.detail ? "unhealthy" : "unhealthy", detail: o.detail };
}

// A Route's spec names what it forwards to as "container/<name>:<port>"
// — the one place the statefile records which container an address
// belongs to. The API serves each resource's yaml, so the console reads
// it here rather than asking for a field to be added to /packages for
// its own convenience (ADR-W027 A1).
const UPSTREAM = /^\s*upstream:\s*["']?container\/([A-Za-z0-9_.-]+)/m;

export function upstreamContainer(routeYaml: string | undefined): string | undefined {
  return routeYaml ? (UPSTREAM.exec(routeYaml)?.[1] ?? undefined) : undefined;
}

export function groupServices(
  pkgs: PackageView[],
  resources: ResourceView[],
  observed: Observed[],
): ServiceGroup[] {
  const health = new Map(observed.map((o) => [`${o.kind}/${o.name}`, o]));

  const byOwner = new Map<string, ResourceView[]>();
  for (const r of resources) {
    const list = byOwner.get(r.owner) ?? [];
    list.push(r);
    byOwner.set(r.owner, list);
  }

  const groups: ServiceGroup[] = [];
  for (const [owner, owned] of byOwner) {
    const label = ownerLabel(owner);
    const pkg = pkgs.find((p) => p.enabled && p.capability === label);

    // Which container each Route forwards to, and where that route
    // answers. The address comes from the package (which resolved the
    // host into an origin); the container comes from the Route's yaml.
    const urls = new Map<string, { url: string; healthy: boolean }>();
    for (const r of owned) {
      if (r.kind !== "Route") continue;
      const container = upstreamContainer(r.yaml);
      const addr = pkg?.addresses?.find((a) => a.route === r.name);
      if (container && addr) urls.set(container, { url: addr.url, healthy: addr.healthy });
    }

    const services: Service[] = owned
      .filter((r) => r.kind === "Container")
      .map((r) => {
        const key = `${r.kind}/${r.name}`;
        const addr = urls.get(r.name);
        return {
          key,
          kind: r.kind,
          name: r.name,
          node: r.node,
          ...healthOf(health.get(key)),
          url: addr?.url,
          urlHealthy: addr?.healthy,
        };
      })
      .sort((a, b) => RANK[a.health] - RANK[b.health] || a.name.localeCompare(b.name));

    // A group is as unwell as its worst container. An owner with no
    // containers at all (ingress routes, system resources) takes the
    // worst of whatever it does own.
    const observable = services.length
      ? services
      : owned.map((r) => ({ ...healthOf(health.get(`${r.kind}/${r.name}`)) }));
    const worst = observable.reduce<Kind>(
      (acc, s) => (RANK[s.health] < RANK[acc] ? s.health : acc),
      "ok",
    );

    groups.push({
      owner,
      label,
      services,
      resourceCount: owned.length,
      health: worst,
      blockers: pkg?.blockers ?? [],
      version: pkg?.installedVersion ?? pkg?.version,
      stale: pkg?.stale,
      catalogVersion: pkg?.stale ? pkg.version : undefined,
    });
  }

  // Unhealthy first: the reason to open Home is to find out what is
  // wrong, and scrolling for it is the current console's failing.
  return groups.sort((a, b) => RANK[a.health] - RANK[b.health] || a.label.localeCompare(b.label));
}
