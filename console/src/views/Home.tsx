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
      <h2>Home</h2>
      <div className="sub">
        {resources.length} resource(s) across {owners.length} owner(s); unhealthy chips are
        outlined red with the observed detail.
      </div>
      {owners.length === 0 && <div className="muted">Statefile is empty — enable a capability.</div>}
      {owners.map((owner) => (
        <div className="card" key={owner}>
          <h3>{owner}</h3>
          <div className="chips">
            {(groups.get(owner) ?? []).map((r) => {
              const o = health.get(`${r.kind}/${r.name}`);
              const bad = o !== undefined && !o.healthy;
              return (
                <span
                  className={`chip${bad ? " bad" : ""}`}
                  key={`${r.kind}/${r.name}`}
                  title={bad ? o.detail : `${r.kind} on ${r.node ?? "-"}`}
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
