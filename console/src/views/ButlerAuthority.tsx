import { useState } from "react";
import {
  agentClassLabel,
  api,
  type AgentClass,
  type CapabilityRequest,
  type ToolControl,
} from "../api";
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

// Recording a person.
//
// The panel could admit, disable and offboard people and had no way to
// record one, so a fresh deployment showed "nobody is recorded yet" and
// offered nothing to do about it. Without this the whole chain below is
// unreachable from the Console: no member means no one to grant a
// Butler to, which means no one who can talk to it.
//
// Who is a CHOICE rather than typed text. The household directory
// already knows who lives here, and a free string invites the one
// mistake this control must not allow quietly: a typo records a person
// who does not exist, they can be admitted and granted a Butler, and
// nothing ever matches them — an access list that looks correct and
// grants nothing to anybody.
//
// The signed-in principal is offered alongside them. It is usually not
// a directory user (the Console holder authenticates as `admin`, not as
// a person in LDAP), and it is exactly who a person setting this up
// first needs to record — so leaving it out would mean the one name
// they need is the one the list refuses to contain.
//
// A child is recorded and never admitted — the store refuses, and so
// does the schema — so the profile is offered rather than assumed, and
// picking child asks for the guardian it requires.
function RecordMember({
  recorded,
  onRecorded,
}: {
  recorded: string[];
  onRecorded: () => void;
}) {
  const people = useLoad(() => api.householdMembers());
  const me = useLoad(() => api.whoami());
  const [name, setName] = useState("");
  const [profile, setProfile] = useState("household");
  const [guardian, setGuardian] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const directory = (people.data ?? []).map((p) => ({
    id: p.username,
    label: p.displayName ? `${p.username} — ${p.displayName}` : p.username,
  }));
  const self = me.data?.name;
  const candidates = [
    ...directory,
    ...(self && !directory.some((d) => d.id === self)
      ? [{ id: self, label: `${self} — you, signed in at this Console` }]
      : []),
  ].filter((c) => !recorded.includes(c.id));

  async function record() {
    setBusy(true);
    setError(null);
    try {
      await api.recordButlerMember(name, profile, guardian.trim() || undefined);
      setName("");
      setGuardian("");
      onRecorded();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  if (candidates.length === 0 && !people.loading) {
    return (
      <div className="muted" style={{ marginTop: 8 }}>
        Everyone this deployment knows about is already recorded.
      </div>
    );
  }

  return (
    <div style={{ marginTop: 8 }}>
      <div className="row" style={{ gap: 8, alignItems: "center" }}>
        <select aria-label="person" value={name} onChange={(e) => setName(e.target.value)}>
          <option value="">Record someone…</option>
          {candidates.map((c) => (
            <option key={c.id} value={c.id}>
              {c.label}
            </option>
          ))}
        </select>
        <select aria-label="profile" value={profile} onChange={(e) => setProfile(e.target.value)}>
          <option value="household">household</option>
          <option value="operator">operator</option>
          <option value="child">child</option>
        </select>
        {profile === "child" && (
          <select aria-label="guardian" value={guardian} onChange={(e) => setGuardian(e.target.value)}>
            <option value="">Guardian…</option>
            {recorded.map((m) => (
              <option key={m} value={m}>
                {m}
              </option>
            ))}
          </select>
        )}
        <button className="btn btn-secondary" disabled={!name || busy} onClick={record}>
          {busy ? "Recording…" : "Record"}
        </button>
      </div>
      <div className="muted" style={{ fontSize: "0.9em", marginTop: 2 }}>
        Recording does not admit. Admit them below once they should be able to use the Butler.
      </div>
      <ErrorLine error={people.error} />
      <ErrorLine error={error} />
    </div>
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
      <RecordMember
        recorded={rows.map((r) => r.member.member)}
        onRecorded={() => members.reload()}
      />
      <ul className="work-evidence">
        {rows.map((row) => (
          <li key={row.member.member}>
            <StatusPill kind={row.active ? "ok" : "mute"}>
              {row.active ? "admitted" : row.member.offboardedAt ? "offboarded" : "not admitted"}
            </StatusPill>
            <strong>{row.member.member}</strong>
            <span className="timeline-src">{row.member.role ?? "member"}</span>
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
                    {row.member.role === "owner" && (
                      <button
                        className="btn btn-ghost"
                        onClick={() => {
                          if (!window.confirm(`Demote ${row.member.member} to Member? Their Console access ends, while Homepage and personal resources remain.`)) return;
                          action.run(async () => {
                            await api.demoteButlerMember(row.member.member);
                            members.reload();
                          }, "Demoted to Member. Console access ended; Member access remains.");
                        }}
                      >
                        Demote to Member
                      </button>
                    )}
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

// The two agent classes (ADR-W033 design §5.2), in the design's own
// words rather than the pre-0058 person vocabulary this used to borrow.
const AGENT_CLASSES: AgentClass[] = ["owner", "member"];

function ToolControls() {
  const controls = useLoad(() => api.butlerToolControls());
  const [agentClass, setAgentClass] = useState<AgentClass>(AGENT_CLASSES[0]);
  const [tool, setTool] = useState("");
  const [reason, setReason] = useState("");
  const [note, setNote] = useState<string | null>(null);
  const action = useAction();

  const rows: ToolControl[] = controls.data?.controls ?? [];
  const set = (state: string) =>
    action.run(async () => {
      const res = await api.setButlerToolControl({ agentClass, tool, state, reason });
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
          <li key={`${c.agentClass}/${c.tool}`}>
            <StatusPill kind={c.state === "disabled" ? "bad" : "ok"}>{c.state}</StatusPill>
            <strong>{c.tool}</strong>
            <span className="timeline-src">{agentClassLabel(c.agentClass)}</span>
            {c.reason && <span className="timeline-src">{c.reason}</span>}
            <span className="timeline-src">
              {c.updatedBy} · {when(c.updatedAt)}
            </span>
            <button
              className="btn btn-ghost"
              onClick={() =>
                action.run(async () => {
                  await api.setButlerToolControl({
                    agentClass: c.agentClass,
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
        <select
          value={agentClass}
          onChange={(e) => setAgentClass(e.target.value as AgentClass)}
        >
          {AGENT_CLASSES.map((c) => (
            <option key={c} value={c}>
              {agentClassLabel(c)}
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
              for {r.requestedBy} · {agentClassLabel(r.agentClass)}
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
