import { Fragment, useState } from "react";
import { api, EnablePreview, PackageView } from "../api";
import { Badge, ErrorLine, useLoad } from "../lib";

// Services (ADR-W021 A5): the catalog, and installing from it.
//
// The form is GENERATED from the package. A package already declares its
// params with required/default, so there is nothing to hand-maintain here
// and nothing to drift when a package gains one.
//
// Install is two steps on purpose. The first is a dry run, which runs
// every check a real proposal runs — validation, standing policy, the
// diff — records none of it, and comes back with the plan and the
// secrets only the operator can supply. That second list is the point:
// meeting a missing secret as a failed reconcile tick, after approving a
// plan that could not have worked, is the shape of F37.
export function Services() {
  const list = useLoad(() => api.packages(), []);
  const [chosen, setChosen] = useState<PackageView | null>(null);
  if (list.error) return <ErrorLine error={list.error} />;
  const pkgs = list.data ?? [];

  return (
    <>
      <h2>Services</h2>
      <div className="sub">
        What this control plane can install. Installing proposes a plan — a human still
        approves it.
      </div>
      <table>
        <thead>
          <tr>
            <th>Package</th>
            <th>Capability</th>
            <th>Version</th>
            <th>State</th>
            <th>Address</th>
            <th>Resources</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {pkgs.map((p) => (
            <Fragment key={`${p.name}@${p.version}`}>
            <tr>
              <td>{p.name}</td>
              <td>
                {p.capability}
                {p.profile ? ` (${p.profile})` : ""}
              </td>
              <td>{p.version}</td>
              <td>{p.enabled ? <Badge value="enabled" /> : "—"}</td>

              {/* Where it is served, and whether it answers. This is the
                  answer to "which port is this on", which previously
                  existed only in <prefix>/routes/ on the node. */}
              <td>
                {p.addresses?.length
                  ? p.addresses.map((a) => (
                      <div key={a.route} className={a.healthy ? "" : "bad"}>
                        <a href={a.url} target="_blank" rel="noreferrer">
                          {a.url}
                        </a>
                        {!a.healthy && (
                          <div className="muted">not answering{a.detail ? `: ${a.detail}` : ""}</div>
                        )}
                      </div>
                    ))
                  : "—"}
              </td>

              <td>
                {p.resources?.length ? (
                  <details>
                    <summary>{p.resources.length}</summary>
                    <div className="chips">
                      {p.resources.map((r) => (
                        <span className="chip" key={`${r.kind}/${r.name}`}>
                          {r.kind}/{r.name}
                        </span>
                      ))}
                    </div>
                  </details>
                ) : (
                  "—"
                )}
              </td>

              <td>
                <button className="action" onClick={() => setChosen(p)}>
                  {p.enabled ? "Reconfigure" : "Install"}
                </button>
              </td>
            </tr>
            {p.blockers?.map((b) => (
              <tr key={b}>
                <td colSpan={6} className="bad">
                  <strong>Will not work here.</strong> {b}
                </td>
              </tr>
            ))}
            </Fragment>
          ))}
        </tbody>
      </table>
      {pkgs.length === 0 && (
        <div className="sub">
          The catalog is empty. Keystone was started without <code>-catalog-dir</code>, or
          this release shipped no packages.
        </div>
      )}
      {chosen && (
        <InstallForm
          pkg={chosen}
          onDone={() => {
            setChosen(null);
            list.reload();
          }}
        />
      )}
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
        <div className="row" key={p.name}>
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
          className="action"
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
            <div className="card bad">
              <strong>This changes sign-in for everyone</strong>
              <div>{preview.authPolicyWarning}</div>
            </div>
          )}
          {preview.providedSecrets.length > 0 && (
            <div className="warn">
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
            className="action"
            disabled={busy}
            onClick={() => run(api.enableCapability(pkg.name, values), onDone)}
          >
            Propose plan
          </button>
        </>
      )}
      <button className="action" disabled={busy} onClick={onDone}>
        Cancel
      </button>
    </div>
  );
}
