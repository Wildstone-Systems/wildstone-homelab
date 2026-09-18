import { useState } from "react";
import { Check } from "@phosphor-icons/react";
import {
  api,
  type ButlerAddress,
  type ButlerRow,
  type ButlerScope,
  type ButlerSurface,
  type IntegrationConnection,
} from "../../api";
import { ErrorLine, useLoad } from "../../lib";
import { StatusPill } from "../../ui/status";
import { Say } from "./ButlerDrawer";

// Why a connection cannot be given to this Butler, or "" when it can.
//
// It is a function rather than a line inside the row because it is the
// A4 crossover rule, and the store enforces the same one with a
// trigger. Two copies of a rule are worth having when one of them is
// the last word and the other only exists so the screen does not offer
// a control that fails — but the copy up here is one that can be read,
// and checked, on its own.
export function withheldFrom(surface: ButlerSurface, c: IntegrationConnection): string {
  if (surface === "family" && c.audience === "private") {
    return `private to ${c.owner} — a household Butler cannot be given it`;
  }
  return "";
}

// Who may talk to it, and what it may read.
//
// The two halves are the same decision seen from two sides, which is
// why they share a tab: a Butler with no people answers nobody, and a
// Butler with no connections has nothing to answer FROM. Both are
// absences rather than refusals — an unticked person does not get a
// "you may not", the Butler simply does not appear for them.
export function TabReach({
  row,
  scope,
  say,
  onChanged,
}: {
  row: ButlerRow;
  scope?: { scope: ButlerScope; people: string[] | null; addresses: ButlerAddress[] | null } | null;
  say: Say;
  onChanged: () => void;
}) {
  const b = row.butler;
  const members = useLoad(() => api.butlerMembers());
  const integrations = useLoad(() => api.integrations());
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const people = scope?.people ?? [];
  const granted = scope?.scope.connections ?? [];
  const addresses = scope?.addresses ?? [];
  const admitted = (members.data?.members ?? [])
    .filter((m) => m.active)
    .map((m) => m.member.member);
  const everyone = Array.from(new Set([...admitted, ...people])).sort();

  async function run(key: string, fn: () => Promise<unknown>, done: string) {
    setBusy(key);
    setError(null);
    try {
      await fn();
      say(done);
      onChanged();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(null);
    }
  }

  return (
    <div className="tab-stack">
      <ErrorLine error={error} />
      <ErrorLine error={members.error} />

      <div>
        <div className="eyebrow">WHO MAY TALK TO IT</div>
        {everyone.length === 0 && (
          <div className="muted">
            Nobody is recorded yet. Record a person on the Butler authority screen first — there is
            nobody to give this Butler to.
          </div>
        )}
        <div className="check-list">
          {everyone.map((m) => {
            const on = people.includes(m);
            const address = addresses.find((a) => a.label === m);
            return (
              <button
                key={m}
                type="button"
                role="checkbox"
                aria-checked={on}
                disabled={busy === m}
                className={`check-row${on ? " on" : ""}`}
                onClick={() =>
                  void run(
                    m,
                    () => api.setButlerAccess(b.name, m, !on),
                    on ? `${m} can no longer reach ${b.name}` : `${m} can now talk to ${b.name}`,
                  )
                }
              >
                <span className="checkbox" aria-hidden>
                  {on && <Check size={11} weight="bold" />}
                </span>
                <span>{m}</span>
                <span className="muted tnum check-note">
                  {address ? `${address.transport} · ${address.externalId}` : "no address linked"}
                </span>
              </button>
            );
          })}
        </div>
        <div className="note">
          {people.length === 0
            ? `Nobody can talk to ${b.name}. It will answer nothing until somebody here is ticked.`
            : "An unticked person cannot reach this Butler at all — it does not appear for them."}
        </div>
      </div>

      <div>
        <div className="eyebrow">CONNECTIONS IT MAY READ</div>
        <ErrorLine error={integrations.error} />
        {(integrations.data?.connections ?? []).length === 0 && !integrations.loading && (
          <div className="muted">Nothing is connected yet, so there is nothing to give it.</div>
        )}
        <div className="chips">
          {(integrations.data?.connections ?? []).map(({ connection: c }) => {
            const on = granted.includes(c.name);
            const no = withheldFrom(b.surface, c);
            if (no) {
              return (
                <span key={c.name} className="chip chip-static" title={no}>
                  {c.name} <span className="chip-count">withheld</span>
                </span>
              );
            }
            return (
              <button
                key={c.name}
                type="button"
                aria-pressed={on}
                disabled={busy === c.name}
                className={on ? "chip on" : "chip"}
                onClick={() =>
                  void run(
                    c.name,
                    () => api.setButlerConnection(b.name, c.name, !on),
                    on
                      ? `${c.name} disconnected — the tools behind it are gone from ${b.name}`
                      : `${c.name} connected — its tools are available to ${b.name}`,
                  )
                }
              >
                {c.name}
                {on && " ✓"}
                {c.state !== "ready" && <StatusPill kind="warn">{c.state}</StatusPill>}
              </button>
            );
          })}
        </div>
        <div className="note">
          A tool exists only while the package behind it is connected — there is no mail tool
          without the mail server. Turning one off takes its tools away from {b.name}.
        </div>
      </div>
    </div>
  );
}
