import { useState } from "react";
import { api, type IntegrationConnection, type Placement } from "../api";
import { ErrorLine, useAction, useLoad } from "../lib";
import { Kind, StatusPill } from "../ui/status";

// Connected applications, and where their adapters run (ADR-W030 A5/
// A5.1, P10/P11).
//
// The screen is built around two things a household actually needs to
// see and cannot get anywhere else.
//
// What a connection is ALLOWED to do, as three separate switches that
// each start off. Connecting an app is not the same as letting it be
// searched, and neither is the same as letting its contents leave the
// house — so they are three checkboxes and not one "enable" button.
//
// Which nodes could take over, and whether they actually could. A
// fallback that has been declared but not prepared is the failure this
// view exists to make visible: it looks like resilience and is not, and
// the moment you find out is the moment the primary goes down.

const STATE_TONE: Record<IntegrationConnection["state"], Kind> = {
  configured: "mute",
  ready: "ok",
  degraded: "warn",
  unavailable: "bad",
};

export function Integrations() {
  const list = useLoad(() => api.integrations());
  const rows = list.data?.connections ?? [];
  return (
    <>
      <div className="sub">
        Applications the Butler can reach. Each starts read-only, live-query-only and local-only:
        being searchable and being allowed to act are separate decisions, made here.
      </div>
      <ErrorLine error={list.error} />
      {rows.length === 0 && !list.loading && (
        <div className="muted">Nothing is connected yet.</div>
      )}
      {rows.map((row) => (
        <Connection key={row.connection.name} connection={row.connection} onChanged={list.reload} />
      ))}
    </>
  );
}

function Connection({
  connection,
  onChanged,
}: {
  connection: IntegrationConnection;
  onChanged: () => void;
}) {
  const placement = useLoad(() => api.integrationPlacement(connection.name), [connection.name]);
  const action = useAction();
  const [note, setNote] = useState<string | null>(null);
  const placements = placement.data?.placements ?? [];
  const secrets = placement.data?.secrets ?? [];

  return (
    <section className="work-section">
      <h3>
        {connection.name}{" "}
        <StatusPill kind={STATE_TONE[connection.state]}>{connection.state}</StatusPill>
      </h3>
      {connection.degradedReason && <div className="msg-note">{connection.degradedReason}</div>}
      <div className="sub">
        {connection.adapter} · {connection.ownership} · owned by {connection.owner}
      </div>

      <div className="work-form">
        {(
          [
            ["actions", connection.actionsEnabled, "can perform actions"],
            ["indexing", connection.indexingEnabled, "is searchable"],
            ["cloud", connection.cloudAllowed, "may reach a cloud model"],
          ] as const
        ).map(([key, on, label]) => (
          <label key={key} className="timeline-src">
            <input
              type="checkbox"
              checked={on}
              onChange={(e) =>
                action.run(async () => {
                  await api.widenIntegration(connection.name, {
                    actions: key === "actions" ? e.target.checked : connection.actionsEnabled,
                    indexing: key === "indexing" ? e.target.checked : connection.indexingEnabled,
                    cloud: key === "cloud" ? e.target.checked : connection.cloudAllowed,
                  });
                  onChanged();
                }, "Saved.")
              }
            />{" "}
            {label}
          </label>
        ))}
      </div>

      <h3>Where it runs</h3>
      {placements.length === 0 && !placement.loading && (
        <div className="muted">No node has been approved to run this adapter.</div>
      )}
      <ul className="work-evidence">
        {placements.map((p) => (
          <PlacementRow key={p.node} placement={p} />
        ))}
      </ul>
      <div className="work-form">
        <button
          className="btn btn-ghost"
          disabled={action.state.phase === "busy"}
          onClick={() =>
            action.run(async () => {
              const res = await api.placeIntegration(connection.name);
              setNote(res.waiting ? (res.note ?? "Waiting.") : `Running on ${res.holder}.`);
              placement.reload();
              onChanged();
            }, "Checked.")
          }
        >
          Check where it should run
        </button>
      </div>
      {note && <div className="msg-note">{note}</div>}

      {secrets.length > 0 && (
        <>
          <h3>Credentials</h3>
          <ul className="work-evidence">
            {secrets.map((s) => (
              <li key={s.seq}>
                <StatusPill kind={s.upstreamReadOnly ? "ok" : "warn"}>
                  {s.upstreamReadOnly ? "read-only" : "wider access"}
                </StatusPill>
                <span>{s.member ? `${s.member}'s account` : "household account"}</span>
                {/* The reference, never the credential: the value lives
                    in the node's secret path and has no route here. */}
                <span className="timeline-src">{s.secretRef}</span>
                {s.disclosure && <span className="timeline-src">{s.disclosure}</span>}
              </li>
            ))}
          </ul>
        </>
      )}
      <ErrorLine error={placement.error} />
      {action.state.phase === "error" && <ErrorLine error={action.state.message} />}
    </section>
  );
}

// A declared node and a prepared one look the same in a list unless you
// say so. This row says so.
function PlacementRow({ placement }: { placement: Placement }) {
  const ready =
    placement.containerRunning && placement.secretProvisioned && placement.readinessVerified;
  const missing = [
    !placement.containerRunning && "its adapter is not running",
    !placement.secretProvisioned && "its credential is not provisioned",
    !placement.readinessVerified && "readiness has not been verified",
  ].filter(Boolean);
  return (
    <li>
      <StatusPill kind={placement.holder ? "ok" : ready ? "mute" : "warn"}>
        {placement.holder ? "running here" : ready ? "ready to take over" : "not ready"}
      </StatusPill>
      <strong>{placement.node}</strong>
      <span className="timeline-src">{placement.role}</span>
      {placement.removing && (
        <span className="timeline-src">
          being removed — the container, credential and revocation each have to be proven gone
        </span>
      )}
      {!ready && !placement.removing && (
        <span className="timeline-src">
          cannot take over: {missing.join(", ")}
        </span>
      )}
    </li>
  );
}
