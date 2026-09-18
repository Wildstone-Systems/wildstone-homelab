import { useState } from "react";
import { DirectoryPicker } from "../ui/DirectoryPicker";
import {
  ArrowCircleUp,
  ArrowSquareOut,
  CaretRight,
  Info,
  WarningCircle,
} from "@phosphor-icons/react";
import { api, EnablePreview, PackageView, ResourceView } from "../api";
import { ErrorLine, useAction, useLoad } from "../lib";
import { Group, GROUPS, groupOf, hostOf, RANK } from "../services";
import { Dot, Kind, StatusPill } from "../ui/status";
import { Drawer } from "../ui/Drawer";

// Services (ADR-W021 A5, ADR-W027): the catalog, and installing from it.
//
// Two halves, because they answer two different questions. What IS
// installed is a status board — where each thing answers and whether it
// does. What COULD be is a decision list, and a row that will not work
// here says so before Install is pressed rather than after a plan is
// approved.
//
// The form below is GENERATED from the package and is unchanged. A
// package already declares its params with required/default, so there
// is nothing to hand-maintain and nothing to drift when a package gains
// one. Install stays two steps: the first is a dry run, which runs every
// check a real proposal runs, records none of it, and comes back with
// the plan and the secrets only the operator can supply. That second
// list is the point — meeting a missing secret as a failed reconcile
// tick, after approving a plan that could not have worked, is the shape
// of F37.
export function Services() {
  const list = useLoad(() => api.packages(), []);
  // Removing a capability proposes deleting everything it owns, and
  // only the statefile knows what that is.
  const resources = useLoad(() => api.resources(), []);
  if (list.error) return <ErrorLine error={list.error} />;
  return (
    <ServicesView
      pkgs={list.data ?? []}
      resources={resources.data ?? []}
      // Without this a failed resources call is indistinguishable from
      // a capability that owns nothing: the drawer said "nothing to
      // remove" and the reason went nowhere.
      resourcesError={resources.error}
      resourcesLoading={resources.loading}
      onInstalled={() => {
        list.reload();
        resources.reload();
      }}
    />
  );
}

// Separated from its fetch so the render check can put the catalog in a
// state: installed and available together, nothing installed yet, and a
// package that will not work here.
// Separated from its fetch so the render check can put the catalog in a
// state: installed and available together, nothing installed yet, a
// package that will not work here, and each group filter.
export function ServicesView({
  pkgs,
  resources = [],
  resourcesError = null,
  resourcesLoading = false,
  // Where the screen opens. Only the render check passes these: the tab
  // and the chips are internal state, and a check that cannot click has
  // no other way to put the board in the state it is asserting about.
  initialTab = "installed",
  initialGroup = "All",
  initialHideBlocked = false,
  onInstalled,
}: {
  pkgs: PackageView[];
  resources?: ResourceView[];
  resourcesError?: string | null;
  resourcesLoading?: boolean;
  initialTab?: "installed" | "catalog";
  initialGroup?: Group | "All";
  initialHideBlocked?: boolean;
  onInstalled: () => void;
}) {
  const [tab, setTab] = useState<"installed" | "catalog">(initialTab);
  const [query, setQuery] = useState("");
  const [group, setGroup] = useState<Group | "All">(initialGroup);
  const [hideBlocked, setHideBlocked] = useState(initialHideBlocked);
  const [chosen, setChosen] = useState<PackageView | null>(null);
  const [detail, setDetail] = useState<PackageView | null>(null);
  const [removing, setRemoving] = useState<PackageView | null>(null);

  // Split on `enabled`, never on name: files-filebrowser is both an
  // installed capability and a catalog entry, and splitting by name
  // makes it vanish from one of them.
  const installedPkgs = pkgs.filter((p) => p.enabled);
  const catalogPkgs = pkgs.filter((p) => !p.enabled);

  // Worst first, then the ones asking to be updated, then by name — the
  // reason to open this tab is to find what is wrong.
  const installed = matching(installedPkgs, query).sort(
    (a, b) =>
      RANK[tileKind(a)] - RANK[tileKind(b)] ||
      Number(!!b.stale) - Number(!!a.stale) ||
      a.name.localeCompare(b.name),
  );

  const catalog = matching(
    catalogPkgs
      .filter((p) => group === "All" || groupOf(p) === group)
      .filter((p) => !(hideBlocked && p.blockers?.length)),
    query,
  ).sort(
    (a, b) =>
      Number(!!a.blockers?.length) - Number(!!b.blockers?.length) || a.name.localeCompare(b.name),
  );

  // Counted from the SAME pool the cards come from, minus the group
  // filter itself. Counting the whole catalog while hiding blocked
  // cards gives a chip that reads 12 and shows 11.
  const pool = hideBlocked ? catalogPkgs.filter((p) => !p.blockers?.length) : catalogPkgs;
  const chips = [
    { label: "All" as const, count: pool.length },
    ...GROUPS.map((g) => ({ label: g, count: pool.filter((p) => groupOf(p) === g).length })),
  ].filter((c) => c.count > 0);

  // Which profile already holds each capability, so a sibling can say
  // so before it is installed alongside one.
  const installedBy = new Map(installedPkgs.map((p) => [p.capability, p.profile ?? p.name]));

  const clearAll = () => {
    setQuery("");
    setGroup("All");
    setHideBlocked(false);
  };

  return (
    <>
      {/* Below the page header and ABOVE the intro paragraph. The other
          way round, the paragraph scrolls under the bar and the first
          row of cards is clipped by it. */}
      <div className="filterbar">
        <div className="filterbar-inner">
          <div className="tabs" role="tablist" aria-label="Services">
            <button
              role="tab"
              aria-selected={tab === "installed"}
              className="tab"
              onClick={() => {
                setTab("installed");
                setQuery("");
              }}
            >
              Installed <span className="tab-count">{installedPkgs.length}</span>
            </button>
            <button
              role="tab"
              aria-selected={tab === "catalog"}
              className="tab"
              onClick={() => {
                setTab("catalog");
                setQuery("");
              }}
            >
              Catalog <span className="tab-count">{catalogPkgs.length}</span>
            </button>
          </div>
          <div className="filterbar-right">
            {tab === "catalog" && (
              <label className="check">
                <input
                  type="checkbox"
                  checked={hideBlocked}
                  onChange={(e) => setHideBlocked(e.target.checked)}
                />
                Hide what won’t work here
              </label>
            )}
            {/* A search typed against Installed means nothing against the
                catalog, so the placeholder names which half it filters
                and switching tabs clears it. */}
            <input
              type="text"
              className="search"
              aria-label={tab === "installed" ? "search installed" : "search the catalog"}
              placeholder={tab === "installed" ? "search installed…" : "search the catalog…"}
              value={query}
              onChange={(e) => setQuery(e.target.value)}
            />
          </div>
        </div>
      </div>

      <div className="sub">
        {tab === "installed"
          ? "What is running in this household — where each capability answers, and whether it does. Worst first."
          : "What this control plane can install. Installing proposes a plan — a human still approves it."}
      </div>

      {tab === "catalog" && chips.length > 1 && (
        <div className="chips group-chips">
          {chips.map((c) => (
            <button
              key={c.label}
              className="chip"
              aria-pressed={group === c.label}
              onClick={() => setGroup(c.label)}
            >
              {c.label} <span className="chip-count">{c.count}</span>
            </button>
          ))}
        </div>
      )}

      <div className="card-grid">
        {tab === "installed"
          ? installed.map((p) => (
              <InstalledTile key={`${p.name}@${p.version}`} pkg={p} onOpen={() => setDetail(p)} />
            ))
          : catalog.map((p) => (
              <CatalogCard
                key={`${p.name}@${p.version}`}
                pkg={p}
                // Only worth saying when it could actually be installed;
                // a blocked card already has a louder thing to read.
                siblingNote={
                  !p.blockers?.length && installedBy.has(p.capability)
                    ? `${p.capability} is already installed as ${installedBy.get(p.capability)} — installing this runs both.`
                    : undefined
                }
                onInstall={() => setChosen(p)}
              />
            ))}
      </div>

      {/* Three different misses, and they are three different
          sentences: an empty catalog is a deployment fact, not a
          filter that matched nothing. */}
      {pkgs.length === 0 ? (
        <div className="sub">
          The catalog is empty. Keystone was started without <code>-catalog-dir</code>, or this
          release shipped no packages.
        </div>
      ) : tab === "installed" && installed.length === 0 ? (
        <div className="muted">
          {query === "" ? (
            "Nothing is installed yet — the Catalog tab is what this control plane can install."
          ) : (
            <>
              Nothing installed matches “{query}”.{" "}
              <button className="btn btn-ghost" onClick={() => setQuery("")}>
                Clear
              </button>
            </>
          )}
        </div>
      ) : tab === "catalog" && catalogPkgs.length === 0 ? (
        // Nothing LEFT to install is not a filter that matched nothing:
        // offering Clear here is offering a button that cannot change
        // the result, which is how a person concludes the screen is
        // broken rather than that they are finished.
        <div className="muted">Everything in the catalog is already installed.</div>
      ) : tab === "catalog" && catalog.length === 0 ? (
        <div className="muted">
          Nothing in the catalog matches these filters.{" "}
          <button className="btn btn-ghost" onClick={clearAll}>
            Clear
          </button>
        </div>
      ) : null}

      {/* The form opens beside the catalog rather than under it. Inline
          at the bottom meant scrolling past every row to reach it, and
          scrolling back to find where you had been — which got worse
          with every package added. */}
      <Drawer open={removing !== null} onClose={() => setRemoving(null)} title={removing?.name} wide>
        {removing && (
          <RemoveFormLoader
            pkg={removing}
            // This PACKAGE's resources. Filtering by capability showed a
            // sibling profile's containers in the list of what a removal
            // would delete — and then deleted them.
            owned={ownedByPackage(resources, removing)}
            // Every owner the statefile actually reported, so an empty
            // result can be read rather than guessed at.
            owners={ownerCounts(resources)}
            error={resourcesError}
            loading={resourcesLoading}
            onDone={() => {
              setRemoving(null);
              onInstalled();
            }}
          />
        )}
      </Drawer>

      <Drawer open={chosen !== null} onClose={() => setChosen(null)} title={chosen?.name} wide>
        {chosen && (
          <InstallForm
            pkg={chosen}
            onDone={() => {
              setChosen(null);
              onInstalled();
            }}
          />
        )}
      </Drawer>

      <Drawer open={detail !== null} onClose={() => setDetail(null)} title={detail?.name} wide>
        {detail && (
          <InstalledDetail
            pkg={detail}
            onReconfigure={() => {
              setChosen(detail);
              setDetail(null);
            }}
            onRemove={() => {
              setRemoving(detail);
              setDetail(null);
            }}
          />
        )}
      </Drawer>
    </>
  );
}

// The app's own logo, over a monogram.
//
// The monogram is UNDERNEATH rather than a fallback branch, so a
// household with no route to the CDN sees two letters instead of a
// broken image — the caveat packages/dashboard/README.md records under
// "Icons come from a CDN", inherited here. aria-hidden because the name
// is right beside it, and a missing logo is never an error state.
//
// The slug comes from Keystone, which resolves it from the same table
// the household dashboard renders from. There is deliberately no
// name-to-slug map in the console: that would be a second copy of it.
const ICON_CDN = "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/svg/";

export function AppLogo({ pkg, size = 28 }: { pkg: PackageView; size?: number }) {
  const mono = (pkg.profile ?? pkg.name).replace(/[^a-z]/gi, "").slice(0, 2).toLowerCase();
  return (
    <span className="card-logo" style={{ width: size, height: size }} aria-hidden="true">
      <span>{mono}</span>
      {pkg.icon && (
        <img
          src={`${ICON_CDN}${pkg.icon}.svg`}
          alt=""
          loading="lazy"
          onError={(e) => {
            (e.currentTarget as HTMLImageElement).style.display = "none";
          }}
        />
      )}
    </span>
  );
}

// How well an installed capability is doing, in the one place both the
// sort and the pill read it from.
export function tileKind(pkg: PackageView): Kind {
  const total = pkg.addresses?.length ?? 0;
  const answering = pkg.addresses?.filter((a) => a.healthy).length ?? 0;
  if (pkg.blockers?.length) return "bad";
  if (total === 0) return "mute";
  return answering === total ? "ok" : "bad";
}

// matching filters the catalog on name and capability. Case-insensitive
// and substring rather than prefix: somebody looking for "jellyfin"
// should find "media-jellyfin".
export function matching(pkgs: PackageView[], query: string): PackageView[] {
  const q = query.trim().toLowerCase();
  if (q === "") return pkgs;
  return pkgs.filter(
    (p) =>
      p.name.toLowerCase().includes(q) ||
      p.capability.toLowerCase().includes(q) ||
      (p.profile ?? "").toLowerCase().includes(q),
  );
}


// An installed capability, as two fixed lines: what it is, whether it
// answers, and one muted line of detail. Everything else — the address
// list, what it owns, Reconfigure and Remove — is behind the card,
// which is the same contract ServiceTile already has on Home.
//
// It used to carry all of that on its face: a row per address, a
// version, a resource count, an optional stale pill, blocker blocks and
// two buttons. Ten installed capabilities was a ragged grid and about
// twenty-four buttons.
export function InstalledTile({ pkg, onOpen }: { pkg: PackageView; onOpen: () => void }) {
  const kind = tileKind(pkg);
  const total = pkg.addresses?.length ?? 0;
  const answering = pkg.addresses?.filter((a) => a.healthy).length ?? 0;
  const state =
    total === 0 ? "no address" : answering === total ? "answering" : `${answering} of ${total} answering`;
  // A phone has no room for "1 of 2 answering" beside a name.
  const shortState = total === 0 ? "internal" : answering === total ? "answering" : `${answering} of ${total}`;
  const addrLine =
    total === 0 ? "internal only" : total === 1 ? hostOf(pkg.addresses![0].url) : `${total} addresses`;
  const metaLine = [
    addrLine,
    pkg.installedVersion ?? pkg.version,
    `${pkg.resources?.length ?? 0} resources`,
  ].join(" · ");
  return (
    <div
      className="card"
      role="button"
      tabIndex={0}
      onClick={onOpen}
      aria-label={`${pkg.name} — ${state}`}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onOpen();
        }
      }}
    >
      <div className="card-head">
        <AppLogo pkg={pkg} />
        <span className="card-name">{pkg.name}</span>
        {/* An upgrade changes the catalog and leaves installed
            capabilities alone, so without this an operator sees no
            change and cannot tell "already current" from "needs
            re-installing" from "the upgrade failed" (F91). The words
            move into the tooltip and the drawer; the arrow is what has
            to survive at this size. */}
        {pkg.stale && (
          <span
            className="card-stale"
            title={`running ${pkg.installedVersion}, catalog has ${pkg.version} — reconfigure to update`}
          >
            <ArrowCircleUp size={14} weight="fill" />
          </span>
        )}
        <StatusPill kind={kind}>
          <span className="wide-only">{state}</span>
          <span className="narrow-only">{shortState}</span>
        </StatusPill>
        <CaretRight size={12} className="muted" />
      </div>
      <div className="card-meta" title={metaLine}>
        {metaLine}
      </div>
    </div>
  );
}

// One thing this control plane could install. What stands in the way is
// stated on the card, before Install is pressed: a blocker cannot be
// answered by filling in a field, so offering the form for one is
// offering a dead end.
export function CatalogCard({
  pkg,
  siblingNote,
  onInstall,
}: {
  pkg: PackageView;
  siblingNote?: string;
  onInstall: () => void;
}) {
  const blocked = (pkg.blockers?.length ?? 0) > 0;
  const required = (pkg.params ?? []).filter((p) => p.required).map((p) => p.name);
  const cap = pkg.capability + (pkg.profile ? ` (${pkg.profile})` : "");
  // What the old table's Needs column said, as the third clause of one
  // line. The generated form is one click away and is where the detail
  // of a param belongs.
  const metaLine = [cap, pkg.version, required.length ? `needs ${required.join(", ")}` : "no setup"].join(
    " · ",
  );
  return (
    <div className={blocked ? "card card-blocked" : "card"}>
      <div className="card-head">
        <AppLogo pkg={pkg} />
        <span className="card-name">{pkg.name}</span>
        <button className="btn btn-secondary btn-sm" disabled={blocked} onClick={onInstall}>
          {blocked ? "Can’t install" : "Install"}
        </button>
      </div>
      <div className="card-meta" title={metaLine}>
        {metaLine}
      </div>
      {/* Two profiles of one capability can run side by side, and
          whether that is what you meant is worth being asked before
          rather than discovered after. */}
      {siblingNote && (
        <div className="card-note">
          <Info size={13} />
          <span>{siblingNote}</span>
        </div>
      )}
      {pkg.blockers?.map((b) => (
        <div className="card-blocker" key={b}>
          <WarningCircle size={14} weight="fill" />
          <span>
            <strong>Won’t work here.</strong> {b}
          </span>
        </div>
      ))}
    </div>
  );
}

// Everything the installed tile used to carry on its face, in the panel
// the tile opens. This is the answer to "which port is this on", which
// before the addresses endpoint existed only in <prefix>/routes/ on the
// node.
export function InstalledDetail({
  pkg,
  onReconfigure,
  onRemove,
}: {
  pkg: PackageView;
  onReconfigure: () => void;
  onRemove?: () => void;
}) {
  const total = pkg.addresses?.length ?? 0;
  return (
    <>
      <div className="drawer-head">
        <AppLogo pkg={pkg} size={26} />
        <h3>{pkg.name}</h3>
        <span className="muted tnum">{pkg.installedVersion ?? pkg.version}</span>
      </div>
      <div className="muted" style={{ marginBottom: "var(--space-4)" }}>
        {pkg.capability}
        {pkg.profile ? ` (${pkg.profile})` : ""}
      </div>

      {pkg.stale && (
        <div className="callout callout-warn">
          Running {pkg.installedVersion}, the catalog has {pkg.version}. Reconfigure to update.
        </div>
      )}
      {pkg.blockers?.map((b) => (
        <div className="blocker" key={b}>
          <strong>Will not work here.</strong> {b}
        </div>
      ))}

      <div className="sect" style={{ marginTop: "var(--space-6)" }}>
        <span className="eyebrow">Where it answers</span>
        <span className="fade" />
      </div>
      {total === 0 ? (
        <div className="muted">
          No address. This capability is reachable only from inside the household network, which is
          a fact about it and not a gap in what we know.
        </div>
      ) : (
        pkg.addresses!.map((a) => (
          <div key={a.route} className="addr">
            <Dot kind={a.healthy ? "ok" : "bad"} />
            <a href={a.url} target="_blank" rel="noreferrer">
              {a.url}
            </a>
            <ArrowSquareOut size={11} />
            {!a.healthy && (
              <div className="danger-line">not answering{a.detail ? `: ${a.detail}` : ""}</div>
            )}
          </div>
        ))
      )}

      <div className="sect" style={{ marginTop: "var(--space-6)" }}>
        <span className="eyebrow">What it owns</span>
        <span className="muted">{pkg.resources?.length ?? 0}</span>
        <span className="fade" />
      </div>
      {pkg.resources?.length ? (
        <div className="chips">
          {pkg.resources.map((r) => (
            <span className="chip tnum" key={`${r.kind}/${r.name}`}>
              {r.kind}/{r.name}
            </span>
          ))}
        </div>
      ) : (
        <div className="muted">Nothing is recorded as owned by this package.</div>
      )}

      <div className="row" style={{ marginTop: "var(--space-8)" }}>
        <button className="btn btn-secondary" onClick={onReconfigure}>
          Reconfigure
        </button>
        {onRemove && (
          <button className="btn btn-ghost" onClick={onRemove}>
            Remove…
          </button>
        )}
      </div>
    </>
  );
}

// Removing a capability is a plan like any other: it proposes deleting
// everything the capability owns, and a human approves it. There is no
// disable endpoint and there should not be one — a side door around the
// human gate is what ground rule 4 exists to prevent.
//
// `wild capability disable` has always built this same body from the
// same two calls; the Console simply never offered it, so a
// half-installed capability could only be cleared from a terminal. That
// matters more than it sounds: a capability that cannot converge blocks
// the WHOLE node, so every later plan fails until it is gone.
// ownerCounts is what the statefile said, by owner. It exists because
// a removal that found nothing is unreadable without it: "nothing is
// owned by capability/X" is the same sentence whether X is misspelt,
// whether the resources are filed under a different owner, or whether
// the list arrived empty for a reason nobody can see. Showing the
// owners that WERE there turns an unexplainable report into one the
// person looking at it can answer.
export function ownerCounts(resources: ResourceView[]): [string, number][] {
  const counts = new Map<string, number>();
  for (const r of resources) counts.set(r.owner, (counts.get(r.owner) ?? 0) + 1);
  return [...counts.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
}

// RemoveFormLoader asks Keystone what the removal would actually do,
// then shows it. The dry run is the same code path the real proposal
// takes, so the preview cannot drift from the plan; a failure to reach
// it falls back to the stamps rather than blocking the removal.
function RemoveFormLoader(props: {
  pkg: PackageView;
  owned: ResourceView[];
  owners?: [string, number][];
  error?: string | null;
  loading?: boolean;
  onDone: () => void;
}) {
  const preview = useLoad(() => api.previewDisablePackage(props.pkg.name), [props.pkg.name]);
  const steps = preview.data?.plan?.steps;
  return (
    <RemoveForm
      {...props}
      plannedDeletes={
        steps
          ? steps
              .filter((st) => st.kind && st.name)
              .map((st) => ({ kind: st.kind as string, name: st.name as string }))
          : undefined
      }
      // Until the dry run answers there is no plan to preview, and the
      // stamp fallback below is a DIFFERENT list — it cannot know what a
      // surviving sibling needs. Passing `loading` alone left the propose
      // button live during that window, so a quick click acted on a list
      // that named Secret/restic-books and a plan that did not.
      previewError={preview.error}
      loading={props.loading || preview.loading}
    />
  );
}

// ownedByPackage is what a removal of THIS package would touch.
//
// It mirrors Keystone's rule, which is the authority: a resource stamped
// with this package's name, or — on a deployment written before
// provenance existed — an unstamped one, but only while no other package
// has claimed the capability. Two profiles installed and an unstamped
// resource between them is ambiguous, and deleting it on a guess is the
// wrong way to be wrong.
export function ownedByPackage(resources: ResourceView[], pkg: PackageView): ResourceView[] {
  const owner = `capability/${pkg.capability}`;
  const mine = resources.filter((r) => r.owner === owner);
  const claimants = new Set(mine.map((r) => r.package).filter((p): p is string => !!p));
  const legacyIsMine = claimants.size === 0 || (claimants.size === 1 && claimants.has(pkg.name));
  return mine.filter((r) => (r.package ? r.package === pkg.name : legacyIsMine));
}

export function RemoveForm({
  pkg,
  owned,
  owners = [],
  plannedDeletes,
  previewError = null,
  error = null,
  loading = false,
  onDone,
}: {
  pkg: PackageView;
  owned: ResourceView[];
  owners?: [string, number][];
  // What Keystone says it will delete, from the endpoint's own dry run.
  // The stamps here cannot answer it alone: a resource a surviving
  // sibling still needs is excluded server-side, so listing by stamp
  // showed Secret/restic-books as deletable and then did not delete it —
  // a destructive-action preview disagreeing with the plan being
  // approved. Absent (no dry run yet, or an older server) falls back to
  // the stamps, which is right whenever nothing is shared.
  plannedDeletes?: { kind: string; name: string }[];
  // Why there is no plan to show. Removal is blocked while this is set:
  // the fallback list is not what would be deleted, and offering a
  // destructive button over the wrong list is worse than offering none.
  previewError?: string | null;
  // "I could not read the statefile" and "this owns nothing" look
  // identical from an empty list, and only one of them is safe to
  // believe.
  error?: string | null;
  loading?: boolean;
  onDone: () => void;
}) {
  const remove = useAction();
  const done = remove.state.phase === "done" ? remove.state : null;
  // Volumes and seeded files are never deleted, so they are never
  // proposed. Keystone refuses a Volume Delete outright (ADR-W004 A4: no
  // Wardstone op exists to delete a volume, and it never auto-deletes
  // data), so a plan containing one can never apply — it fails every
  // pass until F60 sets the step aside, taking the rest of the removal
  // with it. A SeedFile is the same promise one layer up: the file was
  // written into the app's own volume and belongs to the household from
  // the moment it landed, so it survives too. Listing either as
  // "deleted" would be telling the operator something untrue.
  const kept = owned.filter((r) => r.kind === "Volume" || r.kind === "SeedFile");
  const planned = plannedDeletes
    ? new Set(plannedDeletes.map((d) => `${d.kind}/${d.name}`))
    : null;
  const deletable = owned.filter(
    (r) =>
      r.kind !== "Volume" &&
      r.kind !== "SeedFile" &&
      (planned === null || planned.has(`${r.kind}/${r.name}`)),
  );
  // Shown as kept rather than dropped from the list: "this survives
  // because something else needs it" is a fact worth reading before
  // approving a removal.
  const sharedKept = planned
    ? owned.filter(
        (r) =>
          r.kind !== "Volume" &&
          r.kind !== "SeedFile" &&
          !planned.has(`${r.kind}/${r.name}`),
      )
    : [];

  if (done) {
    return (
      <>
        <StatusPill kind="ok">{done.message}</StatusPill>
        <p className="problem-body">
          Nothing has been deleted yet. The plan is waiting on Plans, and the resources go when
          you approve it.
        </p>
        <button className="btn btn-secondary" onClick={onDone}>
          Close
        </button>
      </>
    );
  }

  return (
    <>
      <p className="problem-body">
        This proposes deleting what <strong>{pkg.name}</strong> installed. Another profile of the
        same capability, if one is installed, keeps running — and anything it still needs is left
        alone. Nothing happens until you approve the plan.
      </p>
      {/* Stored data is the part a plan cannot undo. Everything else this
          deletes can be reinstalled; what was stored cannot. */}
      {kept.length > 0 && (
        <div className="callout callout-warn">
          <strong>
            {kept.length === 1 ? "One item stays" : `${kept.length} items stay`}
          </strong>{" "}
          — {kept.map((v) => v.name).join(", ")}. Wildstone never deletes data, so what is
          stored there survives this removal and reinstalling picks it back up. Delete it on the
          node yourself if you want the space.
        </div>
      )}
      <table>
        <thead>
          <tr>
            <th>kind</th>
            <th>name</th>
            <th>node</th>
          </tr>
        </thead>
        <tbody>
          {deletable.map((r) => (
            <tr key={`${r.kind}/${r.name}`}>
              <td>{r.kind}</td>
              <td>{r.name}</td>
              <td>{r.node ?? "—"}</td>
            </tr>
          ))}
          {kept.map((r) => (
            <tr key={`${r.kind}/${r.name}`} style={{ opacity: 0.55 }}>
              <td>{r.kind}</td>
              <td>{r.name}</td>
              <td className="muted">kept</td>
            </tr>
          ))}
        </tbody>
      </table>
      {owned.length === 0 && error !== null && (
        <div className="callout callout-bad">
          <strong>The statefile could not be read</strong>, so what this capability owns is
          unknown — not empty. Nothing is proposed from a list that failed to load.
          <ErrorLine error={error} />
        </div>
      )}
      {owned.length === 0 && error === null && loading && (
        <div className="muted">Reading the statefile…</div>
      )}
      {previewError !== null && (
        <div className="callout callout-bad">
          <strong>Could not work out what removing this would delete.</strong>
          <div>{previewError}</div>
          <div>
            The list below is this package's resources by name, which is not the same as the plan
            — anything a surviving profile still needs is excluded server-side. Close and reopen
            to try again.
          </div>
        </div>
      )}
      {sharedKept.length > 0 && (
        <div className="callout callout-warn">
          <strong>Another profile of this capability still needs these, so they stay:</strong>
          <ul>
            {sharedKept.map((r) => (
              <li key={`${r.kind}/${r.name}`}>
                <code>
                  {r.kind}/{r.name}
                </code>
              </li>
            ))}
          </ul>
        </div>
      )}
      {deletable.length === 0 && kept.length > 0 && error === null && !loading && (
        <div className="muted">
          This capability owns only stored data, which is never deleted. There is nothing to
          propose.
        </div>
      )}
      {owned.length === 0 && error === null && !loading && (
        <div className="muted">
          <p>
            Nothing in the statefile is owned by <code>capability/{pkg.capability}</code>, so
            there is nothing to remove.
          </p>
          {owners.length > 0 ? (
            <>
              <p>
                The statefile reported {resourceCount(owners)} under {owners.length}{" "}
                {owners.length === 1 ? "owner" : "owners"}. If what you expected is here under a
                different name, that is the bug — tell someone, with this list:
              </p>
              <ul className="owner-list">
                {owners.map(([owner, n]) => (
                  <li key={owner}>
                    <code>{owner || "(no owner)"}</code> — {n}
                  </li>
                ))}
              </ul>
            </>
          ) : (
            <p>
              The statefile reported no resources at all, under any owner — which is itself
              worth reporting, because a running deployment has some.
            </p>
          )}
        </div>
      )}
      <div className="row">
        <button
          className="btn btn-danger"
          disabled={
            remove.state.phase === "busy" ||
            deletable.length === 0 ||
            error !== null ||
            loading ||
            previewError !== null
          }
          onClick={() =>
            remove.run(
              () => api.disablePackage(pkg.name),
              "removal proposed — approve it on Plans",
            )
          }
        >
          {remove.state.phase === "busy"
            ? "Proposing…"
            : `Propose removing ${deletable.length === 1 ? "1 resource" : `${deletable.length} resources`}`}
        </button>
        <button className="btn btn-ghost" onClick={onDone}>
          Keep it
        </button>
      </div>
      {remove.state.phase === "error" && <ErrorLine error={remove.state.message} />}
    </>
  );
}

function resourceCount(owners: [string, number][]): string {
  const n = owners.reduce((sum, [, c]) => sum + c, 0);
  return `${n} ${n === 1 ? "resource" : "resources"}`;
}

// Exported for the render check in scripts/: a generated form is the
// part of this screen with real logic, and typecheck cannot tell you it
// renders. Rendering it caught nothing today only because the wire-shape
// bug it would have caught was already found by reading the JSON.
export function InstallForm({ pkg, onDone }: { pkg: PackageView; onDone: () => void }) {
  const params = pkg.params ?? [];
  const [values, setValues] = useState<Record<string, string>>(() =>
    // installed, then suggested, then default. A default is what the
    // PACKAGE suggests to someone installing it for the first time, and
    // suggested is what this DEPLOYMENT knows — but neither is true of a
    // capability that is already running, and offering them is how a
    // reconfigure quietly reverts what the operator chose. Home Assistant
    // was installed with timezone=CST; the package default is UTC; the
    // form offered UTC, and an upgrade would have moved a household's
    // clock with nothing anywhere saying so.
    Object.fromEntries(params.map((p) => [p.name, p.installed ?? p.suggested ?? p.default ?? ""])),
  );
  const [preview, setPreview] = useState<EnablePreview | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // The secrets only the operator can supply, asked for HERE rather than
  // left to a note saying "set each one in Secrets". The note was
  // accurate and did not work: calendar-radicale was approved without
  // its directory password, and every one of its resources sat waiting
  // with the reason in a status column nobody had open.
  const [secretValues, setSecretValues] = useState<Record<string, string>>({});
  // Names already in the node's store, so a reconfigure does not demand
  // a password the operator set months ago. null until known.
  const [existingSecrets, setExistingSecrets] = useState<Set<string> | null>(null);
  const [secretsLater, setSecretsLater] = useState(false);

  const missing = params.filter((p) => p.required && !values[p.name]?.trim());
  const node = values["node"]?.trim() ?? "";
  const provided = preview?.providedSecrets ?? [];
  const unsupplied = provided.filter((s) => !existingSecrets?.has(s) && !secretValues[s]);

  const showPreview = (v: EnablePreview) => {
    setPreview(v);
    setExistingSecrets(null);
    if (v.providedSecrets.length === 0 || !node) return;
    // A failed lookup is treated as "none set": asking for a value that
    // already exists costs a retype, assuming one exists costs a stalled
    // install.
    api
      .nodeSecrets(node)
      .then((r) => setExistingSecrets(new Set(r.secrets.map((x) => x.name))))
      .catch(() => setExistingSecrets(new Set()));
  };

  // Secrets first, then the plan. A secret set for a plan that is then
  // rejected is harmless — nothing reads it — while a plan approved
  // before its secret is the stall this form exists to prevent.
  const propose = async () => {
    if (node) {
      for (const s of provided) {
        const v = secretValues[s];
        if (v) await api.setSecret(node, s, v);
      }
    }
    return api.enableCapability(pkg.name, values);
  };

  const run = (p: Promise<unknown>, after: (v: never) => void) => {
    setBusy(true);
    setError(null);
    p.then((v) => after(v as never))
      .catch((e: Error) => setError(e.message))
      .finally(() => setBusy(false));
  };

  return (
    <div className="panel">
      <h3>
        {pkg.name} {pkg.version}
      </h3>
      {params.map((p) => (
        <div className="field" key={p.name}>
          <label>
            {p.name}
            {p.required ? " *" : ""}
            {p.description && <div className="muted">{p.description}</div>}
          </label>
          {p.source === "directory" ? (
            // A folder, chosen from the node rather than typed. The
            // free-text version of this field is what let "/[REDACTED]"
            // through: the operator was shown a corrupted default and had
            // nothing to check it against.
            <DirectoryPicker
              value={values[p.name] ?? ""}
              node={values["node"] ?? ""}
              onChange={(v) => {
                setValues({ ...values, [p.name]: v });
                setPreview(null);
              }}
            />
          ) : p.options && p.options.length > 0 ? (
            // A choice, not a blank box. The control plane knows the
            // answers; asking the operator to retype one is how a
            // capability ends up on a node that does not exist.
            <select
              value={values[p.name] ?? ""}
              onChange={(e) => {
                setValues({ ...values, [p.name]: e.target.value });
                setPreview(null);
              }}
            >
              {!values[p.name] && <option value="">choose…</option>}
              {p.options.map((o) => (
                <option key={o} value={o}>
                  {o}
                </option>
              ))}
            </select>
          ) : (
            <input
              type="text"
              value={values[p.name] ?? ""}
              // example LAST, and only here — never in the initial value
              // above. suggested and default are answers; an example is
              // only the shape of one, and prefilling it would submit a
              // fabricated address as though the operator had chosen it.
              placeholder={p.installed ?? p.suggested ?? p.default ?? p.example ?? ""}
              onChange={(e) => {
                setValues({ ...values, [p.name]: e.target.value });
                setPreview(null);
              }}
            />
          )}
        </div>
      ))}

      <ErrorLine error={error} />

      {!preview && (
        <button
          className="btn btn-primary"
          disabled={busy || missing.length > 0}
          onClick={() => run(api.previewCapability(pkg.name, values), showPreview)}
        >
          {missing.length > 0 ? `fill in ${missing.map((m) => m.name).join(", ")}` : "Preview"}
        </button>
      )}

      {preview && (
        <>
          <div className="sub">
            {preview.plan?.steps?.length ?? 0} step(s), overall risk{" "}
            {preview.plan?.overallRisk ?? "?"}. Nothing has been recorded yet.
          </div>
          {preview.authPolicyWarning && (
            // Louder than the secrets list on purpose: a missing secret
            // stops one service converging, this changes how the whole
            // household signs in (F77).
            <div className="callout callout-bad">
              <strong>This changes sign-in for everyone</strong>
              <div>{preview.authPolicyWarning}</div>
            </div>
          )}
          {provided.length > 0 && !node && (
            // No node param to store them against: the old instruction is
            // the only honest one left.
            <div className="callout callout-warn">
              <strong>You must supply these secrets before this can converge.</strong> They
              are not generated, because only you can produce a correct value:
              <ul>
                {provided.map((s) => (
                  <li key={s}>
                    <code>secret/{s}</code>
                  </li>
                ))}
              </ul>
              Set each one in Secrets, for the node this installs on.
            </div>
          )}
          {provided.length > 0 && node && (
            <div className="callout callout-warn">
              <strong>This needs {provided.length === 1 ? "a secret" : "secrets"} only you can supply.</strong>{" "}
              They are not generated, because a made-up value would not work. Each is stored on{" "}
              <code>{node}</code> before the plan is proposed.
              {provided.map((s) => {
                const have = existingSecrets?.has(s) ?? false;
                return (
                  <div className="field" key={s}>
                    <label htmlFor={`secret-${s}`}>
                      <code>{s}</code>
                      {preview.secretPurposes?.[s] && <div className="muted">{preview.secretPurposes[s]}</div>}
                    </label>
                    <input
                      id={`secret-${s}`}
                      type="password"
                      autoComplete="new-password"
                      value={secretValues[s] ?? ""}
                      placeholder={have ? "already set — leave blank to keep it" : ""}
                      onChange={(e) => setSecretValues({ ...secretValues, [s]: e.target.value })}
                    />
                  </div>
                );
              })}
              <label className="muted">
                <input
                  type="checkbox"
                  checked={secretsLater}
                  onChange={(e) => setSecretsLater(e.target.checked)}
                />{" "}
                I will set these later in Secrets — the service waits until I do
              </label>
            </div>
          )}
          <button
            className="btn btn-primary"
            disabled={busy || (node !== "" && provided.length > 0 && existingSecrets === null) || (unsupplied.length > 0 && node !== "" && !secretsLater)}
            onClick={() => run(propose(), onDone)}
          >
            {unsupplied.length > 0 && node !== "" && !secretsLater ? `enter ${unsupplied.join(", ")}` : "Propose plan"}
          </button>
        </>
      )}
      <button className="btn btn-secondary" disabled={busy} onClick={onDone}>
        Cancel
      </button>
    </div>
  );
}
