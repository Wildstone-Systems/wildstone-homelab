import { useState } from "react";
import { ArrowSquareOut, Package, WarningCircle } from "@phosphor-icons/react";
import { api, EnablePreview, PackageView, ResourceView } from "../api";
import { ErrorLine, useAction, useLoad } from "../lib";
import { StatusPill } from "../ui/status";
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
export function ServicesView({
  pkgs,
  resources = [],
  onInstalled,
}: {
  pkgs: PackageView[];
  resources?: ResourceView[];
  onInstalled: () => void;
}) {
  const [chosen, setChosen] = useState<PackageView | null>(null);
  const [removing, setRemoving] = useState<PackageView | null>(null);
  const [query, setQuery] = useState("");
  const installed = pkgs.filter((p) => p.enabled);
  const all = pkgs.filter((p) => !p.enabled);
  // Name and capability, because people look for both — "jellyfin" is
  // a package and "media" is what it does, and which one you remember
  // depends on why you are looking.
  const available = matching(all, query);

  return (
    <>
      <div className="sub">
        What this control plane can install. Installing proposes a plan — a human still
        approves it.
      </div>

      {installed.length > 0 && (
        <>
          <div className="sect">
            <span className="eyebrow">Installed</span>
            <span className="muted">{installed.length}</span>
            <span className="fade" />
          </div>
          <div className="tiles" style={{ marginBottom: "var(--space-8)" }}>
            {installed.map((p) => (
              <InstalledTile
                key={`${p.name}@${p.version}`}
                pkg={p}
                onReconfigure={() => setChosen(p)}
                onRemove={() => setRemoving(p)}
              />
            ))}
          </div>
        </>
      )}

      <div className="sect">
        <span className="eyebrow">Available</span>
        <span className="muted">
          {query ? `${available.length} of ${all.length}` : all.length}
        </span>
        <span className="fade" />
        <input
          type="text"
          className="search"
          aria-label="search the catalog"
          placeholder="search…"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
        />
      </div>
      <table>
        <thead>
          <tr>
            <th>Package</th>
            <th>Capability</th>
            <th>Version</th>
            <th>Needs</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {available.map((p) => (
            <AvailableRow key={`${p.name}@${p.version}`} pkg={p} onInstall={() => setChosen(p)} />
          ))}
        </tbody>
      </table>

      {query !== "" && available.length === 0 && (
        <div className="muted">
          Nothing in the catalog matches “{query}”.{" "}
          <button className="btn btn-ghost" onClick={() => setQuery("")}>
            Clear
          </button>
        </div>
      )}
      {pkgs.length === 0 && (
        <div className="sub">
          The catalog is empty. Keystone was started without <code>-catalog-dir</code>, or
          this release shipped no packages.
        </div>
      )}

      {/* The form opens beside the catalog rather than under it. Inline
          at the bottom meant scrolling past every row to reach it, and
          scrolling back to find where you had been — which got worse
          with every package added. */}
      <Drawer open={removing !== null} onClose={() => setRemoving(null)} title={removing?.name} wide>
        {removing && (
          <RemoveForm
            pkg={removing}
            owned={resources.filter((r) => r.owner === `capability/${removing.capability}`)}
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
    </>
  );
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

// An installed capability, as a status board rather than a table row:
// where it answers, whether it does, and what it is made of. This is
// the answer to "which port is this on", which before the addresses
// endpoint existed only in <prefix>/routes/ on the node.
export function InstalledTile({
  pkg,
  onReconfigure,
  onRemove,
}: {
  pkg: PackageView;
  onReconfigure: () => void;
  onRemove?: () => void;
}) {
  const answering = pkg.addresses?.filter((a) => a.healthy).length ?? 0;
  const total = pkg.addresses?.length ?? 0;
  return (
    <div className="tile" style={{ cursor: "default" }}>
      <div className="tile-head">
        <Package size={16} />
        <span className="tile-name">{pkg.name}</span>
      </div>
      <div className="tile-what">
        {pkg.capability}
        {pkg.profile ? ` (${pkg.profile})` : ""}
      </div>

      <div className="tile-foot">
        <StatusPill kind={total === 0 ? "mute" : answering === total ? "ok" : "bad"}>
          {total === 0 ? "no address" : answering === total ? "answering" : `${answering} of ${total} answering`}
        </StatusPill>
      </div>

      {pkg.addresses?.map((a) => (
        <div key={a.route} className="tile-host">
          <a href={a.url} target="_blank" rel="noreferrer">
            {a.url}
          </a>
          <ArrowSquareOut size={11} style={{ marginLeft: 4, verticalAlign: "-1px" }} />
          {!a.healthy && (
            <div className="danger-line">not answering{a.detail ? `: ${a.detail}` : ""}</div>
          )}
        </div>
      ))}

      <div className="tile-meta">
        <span className="tnum">{pkg.installedVersion ?? pkg.version}</span>
        {pkg.resources?.length ? (
          <span>
            {pkg.resources.length} {pkg.resources.length === 1 ? "resource" : "resources"}
          </span>
        ) : null}
      </div>

      {/* An upgrade changes the catalog and leaves installed capabilities
          alone, so without this an operator sees no change and cannot
          tell "already current" from "needs re-installing" from "the
          upgrade failed" (F91). */}
      {pkg.stale && (
        <StatusPill
          kind="warn"
          title={`running ${pkg.installedVersion}, catalog has ${pkg.version} — reconfigure to update`}
        >
          update to {pkg.version}
        </StatusPill>
      )}
      {pkg.blockers?.map((b) => (
        <div className="blocker" key={b}>
          <strong>Will not work here.</strong> {b}
        </div>
      ))}

      <button className="btn btn-secondary btn-block" onClick={onReconfigure}>
        Reconfigure
      </button>
      {onRemove && (
        <button className="btn btn-ghost" onClick={onRemove}>
          Remove…
        </button>
      )}
    </div>
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
export function RemoveForm({
  pkg,
  owned,
  onDone,
}: {
  pkg: PackageView;
  owned: ResourceView[];
  onDone: () => void;
}) {
  const remove = useAction();
  const done = remove.state.phase === "done" ? remove.state : null;
  const volumes = owned.filter((r) => r.kind === "Volume");

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
        This proposes deleting everything <strong>{pkg.capability}</strong> owns. Nothing happens
        until you approve the plan.
      </p>
      {/* Volumes are the part a plan cannot undo. Everything else this
          deletes can be reinstalled; what was stored cannot. */}
      {volumes.length > 0 && (
        <div className="callout callout-bad">
          <strong>
            {volumes.length === 1
              ? "One volume goes with it"
              : `${volumes.length} volumes go with it`}
          </strong>{" "}
          — anything stored in {volumes.map((v) => v.name).join(", ")} is deleted with the
          capability, and no plan brings it back. Take a backup first if you might want it.
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
          {owned.map((r) => (
            <tr key={`${r.kind}/${r.name}`}>
              <td>{r.kind}</td>
              <td>{r.name}</td>
              <td>{r.node ?? "—"}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {owned.length === 0 && (
        <div className="muted">
          Nothing in the statefile is owned by this capability, so there is nothing to remove.
        </div>
      )}
      <div className="row">
        <button
          className="btn btn-danger"
          disabled={remove.state.phase === "busy" || owned.length === 0}
          onClick={() =>
            remove.run(
              () => api.disableCapability(pkg.capability, owned),
              "removal proposed — approve it on Plans",
            )
          }
        >
          {remove.state.phase === "busy"
            ? "Proposing…"
            : `Propose removing ${owned.length === 1 ? "1 resource" : `${owned.length} resources`}`}
        </button>
        <button className="btn btn-ghost" onClick={onDone}>
          Keep it
        </button>
      </div>
      {remove.state.phase === "error" && <ErrorLine error={remove.state.message} />}
    </>
  );
}

// What stands in the way is stated on the row, before Install is
// pressed. A blocker cannot be answered by filling in a field, so
// offering the form for one is offering a dead end.
export function AvailableRow({ pkg, onInstall }: { pkg: PackageView; onInstall: () => void }) {
  const required = (pkg.params ?? []).filter((p) => p.required).map((p) => p.name);
  const blocked = (pkg.blockers?.length ?? 0) > 0;
  return (
    <>
      <tr>
        <td>{pkg.name}</td>
        <td>
          {pkg.capability}
          {pkg.profile ? ` (${pkg.profile})` : ""}
        </td>
        <td className="tnum">{pkg.version}</td>
        <td className="muted">{required.length > 0 ? required.join(", ") : "nothing"}</td>
        <td>
          <button className="btn btn-secondary" disabled={blocked} onClick={onInstall}>
            Install
          </button>
        </td>
      </tr>
      {pkg.blockers?.map((b) => (
        <tr key={b}>
          <td colSpan={5}>
            <div className="blocker">
              <WarningCircle size={14} weight="fill" style={{ verticalAlign: "-2px", marginRight: 6 }} />
              <strong>Will not work here.</strong> {b}
            </div>
          </td>
        </tr>
      ))}
    </>
  );
}

// Exported for the render check in scripts/: a generated form is the
// part of this screen with real logic, and typecheck cannot tell you it
// renders. Rendering it caught nothing today only because the wire-shape
// bug it would have caught was already found by reading the JSON.
export function InstallForm({ pkg, onDone }: { pkg: PackageView; onDone: () => void }) {
  const params = pkg.params ?? [];
  const [values, setValues] = useState<Record<string, string>>(() =>
    // suggested before default: a package's default is a property of the
    // PACKAGE, and suggested is a property of THIS deployment — which
    // node exists, which domain everything else already uses.
    Object.fromEntries(params.map((p) => [p.name, p.suggested ?? p.default ?? ""])),
  );
  const [preview, setPreview] = useState<EnablePreview | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const missing = params.filter((p) => p.required && !values[p.name]?.trim());

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
          {p.options && p.options.length > 0 ? (
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
              placeholder={p.suggested ?? p.default ?? p.example ?? ""}
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
          onClick={() => run(api.previewCapability(pkg.name, values), setPreview)}
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
          {preview.providedSecrets.length > 0 && (
            <div className="callout callout-warn">
              <strong>You must supply these secrets before this can converge.</strong> They
              are not generated, because only you can produce a correct value:
              <ul>
                {preview.providedSecrets.map((s) => (
                  <li key={s}>
                    <code>secret/{s}</code>
                  </li>
                ))}
              </ul>
              Set each one in Secrets, for the node this installs on.
            </div>
          )}
          <button
            className="btn btn-primary"
            disabled={busy}
            onClick={() => run(api.enableCapability(pkg.name, values), onDone)}
          >
            Propose plan
          </button>
        </>
      )}
      <button className="btn btn-secondary" disabled={busy} onClick={onDone}>
        Cancel
      </button>
    </div>
  );
}
