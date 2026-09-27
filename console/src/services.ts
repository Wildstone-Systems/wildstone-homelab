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

// Worst first, everywhere. Exported because the Services board sorts by
// the same order the Home groups do, and two copies of "which is worse"
// is how two screens start disagreeing about it.
export const RANK: Record<Kind, number> = { bad: 0, warn: 1, mute: 2, ok: 3 };

// The catalog is 48 capabilities. A chip per capability is the same
// crowding as the table it replaces, so they collapse into eight
// families — the way a household would say what it is looking for.
export const GROUPS = ["Media", "Home", "Documents", "Network", "Ops", "Identity", "Chat", "AI"] as const;
export type Group = (typeof GROUPS)[number];

// Every capability shipped in packages/ has an explicit row. An
// unmapped one still appears — under Ops — rather than vanishing from
// the catalog, and services.test asserts the map covers the catalog so
// a new package cannot file itself there silently.
const GROUP_OF: Record<string, Group> = {
  media: "Media",
  "movie-automation": "Media",
  "tv-automation": "Media",
  music: "Media",
  "music-automation": "Media",
  subtitles: "Media",
  requests: "Media",
  indexers: "Media",
  downloads: "Media",
  audiobooks: "Media",
  podcasts: "Media",
  books: "Media",
  photos: "Media",

  "home-assistant": "Home",
  "home-automation": "Home",
  cameras: "Home",
  zigbee: "Home",
  mqtt: "Home",
  inventory: "Home",
  recipes: "Home",
  // PantryOS (pantry-pantryos): stock, the grocery list and the
  // cook-along — the kitchen, beside recipes.
  pantry: "Home",
  budget: "Home",
  // Home rather than Identity beside `mail`, and the difference is the
  // capability's whole reason for existing: mail-roundcube's calendars
  // are a surface on somebody else's account, where this one is the
  // household's own store on the household's own disk (ADR-W035 A2). A
  // person looking for it is looking among the things the house runs on,
  // not among the accounts.
  calendar: "Home",
  // The household's shared lists (lists-wildstone): shopping, chores,
  // packing — something the house runs on, beside its calendar, even
  // though Keystone serves it rather than a container.
  lists: "Home",

  documents: "Documents",
  "pdf-tools": "Documents",
  files: "Documents",
  // The household's own drop box (#197). Documents rather than Ops: a
  // family looking for "where the uploads went" looks where the files
  // are, not where the control plane is — even though this one happens
  // to be served by it.
  filedrop: "Documents",
  "file-sync": "Documents",
  bookmarks: "Documents",
  feeds: "Documents",

  ingress: "Network",
  dns: "Network",
  vpn: "Network",
  "captcha-solver": "Network",

  monitoring: "Ops",
  "host-monitoring": "Ops",
  metrics: "Ops",
  timeseries: "Ops",
  notifications: "Ops",
  dashboard: "Ops",
  homarr: "Ops",
  code: "Ops",
  utilities: "Ops",

  identity: "Identity",
  passwords: "Identity",
  mail: "Identity",
  // The Console administers the house rather than being one of the
  // services in it, but it is a package now for the same reason
  // everything else is: so its group is declared where an operator
  // already manages who may reach what.
  console: "Identity",

  "chat-discord": "Chat",
  "chat-matrix": "Chat",
  "matrix-homeserver": "Chat",
  "chat-slack": "Chat",
  "chat-telegram": "Chat",

  "ai-chat": "AI",
  "language-models": "AI",
  // Voice messages to text (ADR-W042): a model the Agents use, beside
  // the language models, not a service a person opens.
  speech: "AI",
  // The household's chat surface for the Butlers it already has (#198).
  // It configures no model and installs nothing; what it is, to a person
  // looking for it, is the assistant.
  assistant: "AI",
  // The cognition behind one Agent (ADR-W039 Phase 6/H5, #351). It is
  // instanced exactly like chat-telegram, but what it groups with is the
  // Butler it thinks for, not the chat adapters: a person looking for
  // "what runs this Agent" is looking among the other AI capabilities,
  // beside assistant and language-models, not among the messaging doors.
  "agent-hermes": "AI",
};

export function groupOf(pkg: { capability: string }): Group {
  return GROUP_OF[pkg.capability] ?? "Ops";
}

// The capabilities the group map claims to cover, for the test that
// compares it against the shipped catalog.
export function mappedCapabilities(): string[] {
  return Object.keys(GROUP_OF).sort();
}

// The host an address is at. A card has room for "jellyfin.home.arpa"
// and not for the scheme and path around it, and the full URL stays in
// the title. Lives here rather than in ServiceTile because the Services
// board and the Home tile now both ask the question.
export function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

export function ownerLabel(owner: string): string {
  return owner.startsWith("capability/") ? owner.slice("capability/".length) : owner;
}

export function healthOf(o: Observed | undefined): { health: Kind; state: string; detail?: string } {
  if (!o) return { health: "mute", state: "unobserved" };
  // Held back by a plan waiting for a human is amber; it is not a fault
  // and must not read as one (F75).
  if (o.awaitingApproval && !o.healthy) {
    return { health: "warn", state: "waiting on a plan", detail: o.detail };
  }
  if (o.healthy) {
    // Converged is not the same as serving. Wildstone's "healthy" means
    // the container matches desired state — its own question, and the
    // right one for it. Whether the thing inside can answer anybody is a
    // different question, and only the app can answer it.
    //
    // open-webui was converged and refusing connections at the same
    // time, for several minutes, while every surface read 0 unhealthy
    // and the dashboard offered a link that would not load.
    switch (o.appHealth) {
      case "starting":
        return { health: "warn", state: "starting up", detail: o.detail };
      case "unhealthy":
        return { health: "warn", state: "running, not ready", detail: o.detail };
      // "healthy" and absent both mean nothing is wrong. Most images
      // declare no healthcheck at all, and silence must never render as
      // a problem.
      default:
        return { health: "ok", state: "healthy", detail: o.detail };
    }
  }
  return { health: "bad", state: "unhealthy", detail: o.detail };
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
