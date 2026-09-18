import { useState } from "react";
import { api, type ButlerRow, type ButlerTools, type CapabilityTool } from "../../api";
import { ErrorLine } from "../../lib";
import { StatusPill } from "../../ui/status";
import { Say } from "./ButlerDrawer";

// What this Butler may DO.
//
// Every row comes from the server, and that is the point rather than an
// implementation detail. The tool list is the gateway's own registry,
// so a screen cannot offer a switch for a tool that does not exist or
// miss one that does; and whether a tool is available is the
// effective-authority resolver's answer, so a screen cannot show as on
// something a dispatch would refuse. Both were possible to get wrong
// from the client, and both would be wrong in the direction that
// matters — a person believing they had turned something off.
//
// The control is on/off because that is what the engine has. A Butler
// narrows its profile and never widens it, so "give it back" returns a
// tool to whatever the profile allows rather than granting it outright.
// There is deliberately no third "ask first" state: the Butler loop has
// no such thing, and a control the engine ignores is worse than no
// control at all.
export function TabTools({
  row,
  tools,
  error: loadError,
  say,
  onChanged,
}: {
  row: ButlerRow;
  tools?: ButlerTools | null;
  error: string | null;
  say: Say;
  onChanged: () => void;
}) {
  const b = row.butler;
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const rows = tools?.tools ?? [];
  const narrowed = tools?.narrowedTools ?? {};
  const on = rows.filter((t) => t.available).length;
  const unwired = rows.filter((t) => !t.available && narrowed[t.name] === undefined);

  async function set(tool: string, allow: boolean) {
    setBusy(tool);
    setError(null);
    try {
      await api.setButlerTool(b.name, tool, allow);
      say(
        allow
          ? `${tool} is back — ${b.name} may use it again`
          : `${tool} taken away from ${b.name}`,
        allow ? "ok" : "warn",
      );
      onChanged();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(null);
    }
  }

  return (
    <div className="tab-stack">
      <div className="row tools-head">
        <div className="eyebrow">
          TOOLS — {on} of {rows.length} allowed
        </div>
        <div className="muted tools-profile">{tools?.profile} profile</div>
      </div>
      <div className="note">
        These are the {tools?.profile ?? ""} surface's tools. A switch here can take one away and
        give it back; it can never add one this surface does not have.
      </div>

      <ErrorLine error={loadError} />
      <ErrorLine error={error} />

      {tools?.modelVerdict === "qa-only" && (
        <div className="warn-panel">
          <StatusPill kind="warn">questions only</StatusPill> Its model failed the tool check, so
          anything that does something is withheld until the route passes. That is the route's
          verdict, not a switch on this tab.
        </div>
      )}

      {rows.length > 0 && on === 0 && (
        <div className="warn-panel">
          <StatusPill kind="warn">nothing to hand</StatusPill> {b.name} will still talk, but it
          cannot look anything up.
        </div>
      )}

      <div className="tool-list">
        {rows.map((t) => (
          <Tool
            key={t.name}
            tool={t}
            narrowedReason={narrowed[t.name]}
            busy={busy === t.name}
            onSet={(allow) => void set(t.name, allow)}
          />
        ))}
      </div>

      {unwired.length > 0 && (
        <div className="tool-locked-section">
          <div className="eyebrow">
            {unwired.length} {unwired.length === 1 ? "TOOL NEEDS" : "TOOLS NEED"} SOMETHING THIS
            HOUSE HAS NOT SET UP
          </div>
          <div className="note">
            These are not switched off — nothing here provides them yet. Connect the package behind
            one on the Reach tab and it moves into the list above.
          </div>
        </div>
      )}
    </div>
  );
}

// One tool, in the three states it can actually be in.
//
// "Off" and "unavailable" are kept apart because only the first is
// something this screen did and only the first is something it can
// undo. A tool nothing here has wired offers no switch at all — a
// button that cannot work is a worse answer than the sentence saying
// why.
export function Tool({
  tool,
  narrowedReason,
  busy,
  onSet,
}: {
  tool: CapabilityTool;
  narrowedReason?: string;
  busy: boolean;
  onSet: (allow: boolean) => void;
}) {
  const off = narrowedReason !== undefined;
  return (
    <div className={`tool-row${off || tool.available ? "" : " tool-locked"}`}>
      <div className="cell">
        <div className="row tool-name">
          <code>{tool.name}</code>
          {off ? (
            <StatusPill kind="mute">off</StatusPill>
          ) : tool.available ? (
            <StatusPill kind="ok">on</StatusPill>
          ) : (
            <StatusPill kind="warn">unavailable</StatusPill>
          )}
          {tool.mutating && <StatusPill kind="warn">does something</StatusPill>}
          {tool.grantRequired && <span className="muted">needs a grant</span>}
        </div>
        <div className="muted">{tool.summary}</div>
        {off && narrowedReason && <div className="muted tool-why">{narrowedReason}</div>}
        {!off && !tool.available && tool.reason && (
          <div className="muted tool-why">{tool.reason}</div>
        )}
      </div>
      {(off || tool.available) && (
        <div className="tri">
          <button
            type="button"
            aria-pressed={!off}
            disabled={busy}
            onClick={() => !off || onSet(true)}
          >
            on
          </button>
          <button
            type="button"
            aria-pressed={off}
            disabled={busy}
            onClick={() => off || onSet(false)}
          >
            off
          </button>
        </div>
      )}
    </div>
  );
}
