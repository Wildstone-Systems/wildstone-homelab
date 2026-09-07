import { api, Observed, ResourceView } from "../api";
import { ErrorLine, useLoad } from "../lib";

// Home (ADR-W016 A2): capability-centric — resources grouped by owner,
// each chip carrying observed health. "Is my house okay" in one screen.
export function Home() {
  const res = useLoad(() => Promise.all([api.resources(), api.observed()]));
  if (res.error) return <ErrorLine error={res.error} />;
  const [resources, observed] = res.data ?? [[], []];

  const health = new Map<string, Observed>();
  for (const o of observed) health.set(`${o.kind}/${o.name}`, o);

  const groups = new Map<string, ResourceView[]>();
  for (const r of resources) {
    const list = groups.get(r.owner) ?? [];
    list.push(r);
    groups.set(r.owner, list);
  }
  const owners = [...groups.keys()].sort();

  return (
    <>
      <div className="sub">
        {resources.length} resource(s) across {owners.length} owner(s). Red means something is
        wrong; amber means a plan is waiting for your approval. Hover for the detail.
      </div>
      {owners.length === 0 && <div className="muted">Statefile is empty — enable a capability.</div>}
      {owners.map((owner) => (
        <div className="card" key={owner}>
          <h3>{owner}</h3>
          <div className="chips">
            {(groups.get(owner) ?? []).map((r) => {
              const o = health.get(`${r.kind}/${r.name}`);
              // Three states, not two (F75). A plan waiting for a
              // human is not a fault, and colouring it the same red as
              // a broken service is how a restart made a working
              // homelab look like it had fallen over.
              const waiting = o !== undefined && !o.healthy && o.awaitingApproval === true;
              const bad = o !== undefined && !o.healthy && !waiting;
              return (
                <span
                  className={`chip${bad ? " bad" : ""}${waiting ? " waiting" : ""}`}
                  key={`${r.kind}/${r.name}`}
                  title={bad || waiting ? o.detail : `${r.kind} on ${r.node ?? "-"}`}
                >
                  {r.kind}/{r.name}
                </span>
              );
            })}
          </div>
        </div>
      ))}
    </>
  );
}
