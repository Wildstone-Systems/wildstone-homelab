import { useState } from "react";
import { api, type CapabilityRequest, type ToolControl } from "../api";
import { ErrorLine, useAction, useLoad, when } from "../lib";
import { StatusPill } from "../ui/status";

// Who the Butler is for, and what it may do (ADR-W030 A2.1/A7/A8, P7).
//
// Three lists, in the order an administrator actually thinks about them:
// who is admitted, what is switched off, and what has been asked for.
//
// The screen is careful about one thing above all. A control here can
// turn an existing capability off and back on; it can never ADD one. The
// household Butler's tool map is its structural ceiling, and no switch
// on this page reaches past it — so approving a request and enabling a
// tool are separate acts, and the API says so out loud when a switch
// would do nothing.

export function ButlerAuthority() {
  return (
    <>
      <div className="sub">
        Butler admission is separate from a household account: disabling someone's Butler and
        deleting their directory account are different acts, and this page does the first. Switches
        here turn existing capabilities off and on — none of them can add one.
      </div>
      <Members />
      <ToolControls />
      <Requests />
    </>
  );
}

function Members() {
  const members = useLoad(() => api.butlerMembers());
  const action = useAction();
  const rows = members.data?.members ?? [];
  return (
    <section className="work-section">
      <h3>Admitted to the Butler</h3>
      <ErrorLine error={members.error} />
      {rows.length === 0 && !members.loading && (
        <div className="muted">
          Nobody is recorded yet. A person is recorded first and admitted second, because existing
          and being allowed to use the Butler are different facts.
        </div>
      )}
      <ul className="work-evidence">
        {rows.map((row) => (
          <li key={row.member.member}>
            <StatusPill kind={row.active ? "ok" : "mute"}>
              {row.active ? "admitted" : row.member.offboardedAt ? "offboarded" : "not admitted"}
            </StatusPill>
            <strong>{row.member.member}</strong>
            <span className="timeline-src">{row.member.profile}</span>
            {row.member.guardian && (
              <span className="timeline-src">guardian: {row.member.guardian}</span>
            )}
            <span className="timeline-src">
              {(row.links ?? []).filter((l) => !l.revokedAt).length} linked{" "}
              {(row.links ?? []).filter((l) => !l.revokedAt).length === 1 ? "identity" : "identities"}
            </span>
            {row.member.profile !== "child" && (
              <span className="work-form">
                {!row.active && (
                  <button
                    className="btn btn-ghost"
                    onClick={() =>
                      action.run(async () => {
                        await api.admitButlerMember(row.member.member);
                        members.reload();
                      }, "Admitted.")
                    }
                  >
                    Admit
                  </button>
                )}
                {row.active && (
                  <>
                    <button
                      className="btn btn-ghost"
                      onClick={() =>
                        action.run(async () => {
                          await api.disableButlerMember(row.member.member);
                          members.reload();
                        }, "Disabled.")
                      }
                    >
                      Disable
                    </button>
                    <button
                      className="btn btn-ghost"
                      onClick={() =>
                        action.run(async () => {
                          const res = await api.offboardButlerMember(row.member.member);
                          members.reload();
                          return res;
                        }, "Offboarded. Access is denied now; cleanup follows as plans.")
                      }
                    >
                      Offboard
                    </button>
                  </>
                )}
              </span>
            )}
            {row.member.profile === "child" && (
              <span className="timeline-src">
                the child surface is not enabled, so this record cannot be admitted
              </span>
            )}
          </li>
        ))}
      </ul>
      {action.state.phase === "error" && <ErrorLine error={action.state.message} />}
      {action.state.phase === "done" && <div className="msg-note">{action.state.message}</div>}
    </section>
  );
}

const PROFILES = ["operator", "household"];

function ToolControls() {
  const controls = useLoad(() => api.butlerToolControls());
  const [profile, setProfile] = useState(PROFILES[0]);
  const [tool, setTool] = useState("");
  const [reason, setReason] = useState("");
  const [note, setNote] = useState<string | null>(null);
  const action = useAction();

  const rows: ToolControl[] = controls.data?.controls ?? [];
  const set = (state: string) =>
    action.run(async () => {
      const res = await api.setButlerToolControl({ profile, tool, state, reason });
      setNote(res.note ?? null);
      setTool("");
      setReason("");
      controls.reload();
    }, state === "" ? "Cleared." : `Set to ${state}.`);

  return (
    <section className="work-section">
      <h3>Tool switches</h3>
      <ErrorLine error={controls.error} />
      {rows.length === 0 && !controls.loading && (
        <div className="muted">
          No switches set. Every tool a surface has is following its own default.
        </div>
      )}
      <ul className="work-evidence">
        {rows.map((c) => (
          <li key={`${c.profile}/${c.tool}`}>
            <StatusPill kind={c.state === "disabled" ? "bad" : "ok"}>{c.state}</StatusPill>
            <strong>{c.tool}</strong>
            <span className="timeline-src">{c.profile}</span>
            {c.reason && <span className="timeline-src">{c.reason}</span>}
            <span className="timeline-src">
              {c.updatedBy} · {when(c.updatedAt)}
            </span>
            <button
              className="btn btn-ghost"
              onClick={() =>
                action.run(async () => {
                  await api.setButlerToolControl({
                    profile: c.profile,
                    tool: c.tool,
                    state: "",
                    reason: "",
                  });
                  controls.reload();
                }, "Back to its default.")
              }
            >
              Clear
            </button>
          </li>
        ))}
      </ul>
      <div className="work-form">
        <select value={profile} onChange={(e) => setProfile(e.target.value)}>
          {PROFILES.map((p) => (
            <option key={p} value={p}>
              {p}
            </option>
          ))}
        </select>
        <input value={tool} placeholder="tool name" onChange={(e) => setTool(e.target.value)} />
        <input
          value={reason}
          placeholder="Why (shown to whoever asks)"
          onChange={(e) => setReason(e.target.value)}
        />
        <button className="btn" disabled={!tool} onClick={() => set("disabled")}>
          Turn off
        </button>
        <button className="btn btn-ghost" disabled={!tool} onClick={() => set("enabled")}>
          Turn on
        </button>
      </div>
      {note && <div className="msg-note">{note}</div>}
      {action.state.phase === "error" && <ErrorLine error={action.state.message} />}
    </section>
  );
}

function Requests() {
  const requests = useLoad(() => api.capabilityRequests());
  const action = useAction();
  const [note, setNote] = useState<string | null>(null);
  const rows: CapabilityRequest[] = requests.data?.requests ?? [];
  return (
    <section className="work-section">
      <h3>Asked for</h3>
      <ErrorLine error={requests.error} />
      {rows.length === 0 && !requests.loading && (
        <div className="muted">Nothing has been requested.</div>
      )}
      <ul className="work-evidence">
        {rows.map((r) => (
          <li key={r.seq}>
            <StatusPill kind={r.state === "open" ? "warn" : "mute"}>{r.state}</StatusPill>
            <strong>{r.tool || r.kind}</strong>
            <span>{r.description}</span>
            <span className="timeline-src">
              for {r.requestedBy} · {r.profile}
            </span>
            {r.state === "open" && (
              <span className="work-form">
                <button
                  className="btn btn-ghost"
                  onClick={() =>
                    action.run(async () => {
                      const res = await api.decideCapabilityRequest(r.seq, "approved", "");
                      setNote(res.note);
                      requests.reload();
                    }, "Recorded.")
                  }
                >
                  Approve
                </button>
                <button
                  className="btn btn-ghost"
                  onClick={() =>
                    action.run(async () => {
                      await api.decideCapabilityRequest(r.seq, "declined", "");
                      requests.reload();
                    }, "Declined.")
                  }
                >
                  Decline
                </button>
              </span>
            )}
            {r.decisionNote && <span className="timeline-src">{r.decisionNote}</span>}
          </li>
        ))}
      </ul>
      {note && <div className="msg-note">{note}</div>}
      {action.state.phase === "error" && <ErrorLine error={action.state.message} />}
    </section>
  );
}
