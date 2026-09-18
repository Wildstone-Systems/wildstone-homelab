import { useState } from "react";
import { api, type ButlerRow } from "../../api";
import { ErrorLine, when } from "../../lib";
import { Say } from "./ButlerDrawer";

// What this Butler is for, in its owner's words.
//
// The note under the box is doing real work. A free-text field sitting
// next to a list of tools invites the belief that writing "you may read
// the calendar" in it does something — it does not, and A9 says so:
// a directive shapes how a Butler TALKS and grants it nothing. Reach,
// tools and memory are set on their own tabs, and this says which.
//
// The name is shown and not editable, and the reason is in the note.
// The Butler's key is its name, and four tables — its memory, its
// index, the routines assigned to it and its durable tasks — reference
// it as text with no foreign key to follow. Renaming would orphan the
// lot, and orphaning a Butler's MEMORY is exactly the material
// ADR-W031 says survives even retirement.
export function TabIdentity({
  row,
  say,
  onChanged,
}: {
  row: ButlerRow;
  say: Say;
  onChanged: () => void;
}) {
  const b = row.butler;
  const [directive, setDirective] = useState(b.directive ?? "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const dirty = directive !== (b.directive ?? "");

  async function save() {
    setBusy(true);
    setError(null);
    try {
      await api.setButlerDirective(b.name, directive);
      say(`${b.name} rewritten`);
      onChanged();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="tab-stack">
      <div>
        <label htmlFor="butler-name">Name</label>
        <input id="butler-name" value={b.name} readOnly />
        <div className="note">
          A Butler's name is how everything else refers to it — its memory, its routines and its
          work are all filed under it. Duplicate it under a new name instead.
        </div>
      </div>

      <div>
        <label htmlFor="butler-directive">What it is for</label>
        <textarea
          id="butler-directive"
          rows={4}
          value={directive}
          disabled={!!b.retiredAt}
          onChange={(e) => setDirective(e.target.value)}
        />
        <div className="note">
          Shapes how it talks. It grants nothing — reach, tools and memory are set on their own
          tabs.
        </div>
      </div>

      <div className="card">
        <div className="eyebrow">SURFACE</div>
        <div>
          {b.surface === "admin" ? "Operator" : "Household"} —{" "}
          {b.surface === "admin"
            ? "may propose home lab changes, never approves them"
            : "reads the home, cannot touch the home lab"}
        </div>
        <div className="note">
          A surface is granted at creation and cannot be changed. Duplicate this Butler onto the
          other surface instead.
        </div>
      </div>

      <div className="muted tnum">
        created by {b.createdBy} on {when(b.createdAt)}
        {b.updatedBy && ` · last changed by ${b.updatedBy} ${when(b.updatedAt)}`}
      </div>

      <ErrorLine error={error} />
      <div className="row">
        <button className="btn btn-primary" disabled={!dirty || busy || !!b.retiredAt} onClick={save}>
          {busy ? "Saving…" : "Save"}
        </button>
      </div>
    </div>
  );
}
