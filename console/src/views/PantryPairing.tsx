import { useState } from "react";
import {
  api,
  isPendingStepUp,
  PANTRY_GROCERY_MOVE_PATH,
  PANTRY_PAIRING_PATH,
  type GroceryMoveList,
  type GroceryMoveResult,
  type PantryPairingStatus,
} from "../api";
import { ErrorLine, useAction, useLoad, when } from "../lib";
import { StatusPill } from "../ui/status";

// Pairing with PantryOS (ADR-W042 A6, #384), in the pantry service's
// detail panel.
//
// ONE CODE, NO SECRETS. PantryOS shows a one-time code; the Owner types it
// here with PantryOS's address. Keystone and PantryOS then swap a publisher
// token and an agent key machine to machine, and neither is ever shown —
// not here, not in the response, not afterwards.
//
// A CONFIRMED PASSWORD IS NOT A LOST FORM. Pairing is consequential, so it
// may send the Owner off to confirm their password first. What they typed
// is kept for this tab only, and the pairing is one click from done when
// they come back — the code lives five minutes, which is long enough.
const DRAFT_KEY = "wst.console.pantry-pairing-draft";

type Draft = { address: string; code: string; keystoneUrl: string };

function loadDraft(): Draft | null {
  try {
    const raw = sessionStorage.getItem(DRAFT_KEY);
    return raw ? (JSON.parse(raw) as Draft) : null;
  } catch {
    return null;
  }
}

function saveDraft(d: Draft | null) {
  try {
    if (d) sessionStorage.setItem(DRAFT_KEY, JSON.stringify(d));
    else sessionStorage.removeItem(DRAFT_KEY);
  } catch {
    /* private mode: the form simply is not restored */
  }
}

export function PantryPairingPanel() {
  const status = useLoad(() => api.pantryPairing(), []);
  if (status.error) return <ErrorLine error={status.error} />;
  if (!status.data) return <div className="muted">Checking whether PantryOS is paired…</div>;
  return (
    <>
      <PantryPairingView status={status.data} onChanged={() => status.reload()} />
      {status.data.paired && <GroceryMovePanel />}
    </>
  );
}

// Separated from its fetch so the render check can put it in each state:
// not paired, paired, and back from a password confirmation.
export function PantryPairingView({
  status,
  onChanged,
  origin = typeof window !== "undefined" ? window.location.origin : "",
}: {
  status: PantryPairingStatus;
  onChanged: () => void;
  origin?: string;
}) {
  const resumingPair = isPendingStepUp("POST", PANTRY_PAIRING_PATH);
  const [draft, setDraft] = useState<Draft>(
    () => (resumingPair ? loadDraft() : null) ?? { address: "", code: "", keystoneUrl: origin },
  );
  const [pairing, setPairing] = useState(!status.paired || resumingPair);
  const [confirmUnpair, setConfirmUnpair] = useState(() => isPendingStepUp("DELETE", PANTRY_PAIRING_PATH));
  const pair = useAction();
  const unpair = useAction();
  const busy = pair.state.phase === "busy" || unpair.state.phase === "busy";
  const ready = draft.address.trim() !== "" && draft.code.trim() !== "" && draft.keystoneUrl.trim() !== "";

  const submit = () => {
    saveDraft(draft);
    pair.run(async () => {
      await api.pairPantry({
        address: draft.address.trim(),
        code: draft.code.trim(),
        keystoneUrl: draft.keystoneUrl.trim(),
      });
      saveDraft(null);
      setPairing(false);
      setDraft((d) => ({ ...d, code: "" }));
      onChanged();
    }, "paired");
  };

  return (
    <>
      <div className="sect" style={{ marginTop: "var(--space-6)" }}>
        <span className="eyebrow">Connection</span>
        <StatusPill kind={status.paired ? "ok" : "mute"}>{status.paired ? "paired" : "not paired"}</StatusPill>
        <span className="fade" />
      </div>

      {status.paired && (
        <div>
          Reaching PantryOS at <strong>{status.pairing.endpoint}</strong>
          <div className="muted">
            Paired {when(status.pairing.pairedAt)} by {status.pairing.pairedBy}.
          </div>
        </div>
      )}

      {pairing ? (
        <div style={{ display: "grid", gap: "var(--space-3)", marginTop: "var(--space-4)" }}>
          {resumingPair && (
            <div className="callout">Password confirmed. Pair now to finish — the code is still what you typed.</div>
          )}
          <div className="note">
            In PantryOS, open Admin → Integrations → Wildstone Home Lab and choose{" "}
            <strong>{status.paired ? "Pair again" : "Pair with Wildstone"}</strong>. It shows a code and its own
            address. The code works once and lasts five minutes.
          </div>
          <div className="field" style={{ marginBottom: 0 }}>
            <label htmlFor="pantry-address">PantryOS address</label>
            <input
              id="pantry-address"
              type="text"
              value={draft.address}
              placeholder="e.g. https://pantry.example.internal"
              onChange={(e) => setDraft((d) => ({ ...d, address: e.target.value }))}
            />
          </div>
          <div className="field" style={{ marginBottom: 0 }}>
            <label htmlFor="pantry-code">Pairing code</label>
            <input
              id="pantry-code"
              type="text"
              value={draft.code}
              placeholder="ABCD-EFGH"
              autoComplete="off"
              onChange={(e) => setDraft((d) => ({ ...d, code: e.target.value }))}
            />
          </div>
          <div className="field" style={{ marginBottom: 0 }}>
            <label htmlFor="pantry-keystone">This Wildstone's address, as PantryOS reaches it</label>
            <input
              id="pantry-keystone"
              type="text"
              value={draft.keystoneUrl}
              onChange={(e) => setDraft((d) => ({ ...d, keystoneUrl: e.target.value }))}
            />
            <div className="note">
              Where PantryOS sends its events (meals, groceries, low stock). Usually this Console's address; change
              it if PantryOS's machine reaches Wildstone by another name.
            </div>
          </div>
          <div className="row">
            <button className="btn btn-primary" disabled={!ready || busy} onClick={submit}>
              {pair.state.phase === "busy" ? "Pairing…" : status.paired ? "Pair again" : "Pair PantryOS"}
            </button>
            {status.paired && (
              <button
                className="btn btn-ghost"
                disabled={busy}
                onClick={() => {
                  saveDraft(null);
                  setPairing(false);
                }}
              >
                Cancel
              </button>
            )}
          </div>
          {pair.state.phase === "error" && <ErrorLine error={pair.state.message} />}
        </div>
      ) : (
        <div className="row" style={{ marginTop: "var(--space-4)" }}>
          {pair.state.phase === "done" && <span className="muted">Paired. The pantry tools are ready.</span>}
          <button className="btn btn-secondary" disabled={busy} onClick={() => setPairing(true)}>
            Pair again…
          </button>
          {confirmUnpair ? (
            <>
              <span className="muted">The pantry tools stop and PantryOS can no longer send events.</span>
              <button
                className="btn btn-danger"
                disabled={busy}
                onClick={() =>
                  unpair.run(async () => {
                    await api.unpairPantry();
                    setConfirmUnpair(false);
                    setPairing(true);
                    onChanged();
                  }, "unpaired")
                }
              >
                {unpair.state.phase === "busy" ? "Unpairing…" : "Unpair"}
              </button>
              <button className="btn btn-ghost" disabled={busy} onClick={() => setConfirmUnpair(false)}>
                Keep it
              </button>
            </>
          ) : (
            <button className="btn btn-ghost" disabled={busy} onClick={() => setConfirmUnpair(true)}>
              Unpair…
            </button>
          )}
        </div>
      )}
      {unpair.state.phase === "error" && <ErrorLine error={unpair.state.message} />}
    </>
  );
}

// Moving the old grocery list (#387). While PantryOS is installed it owns
// the grocery list, so a Wildstone "Shopping" list nobody's requests
// reach any more is offered across once PantryOS is paired — offered,
// never done on its own. Each item leaves the old list only once PantryOS
// has it; the old list goes when nothing open is left on it.
export function GroceryMovePanel() {
  const preview = useLoad(() => api.groceryMovePreview(), []);
  if (preview.error) return <ErrorLine error={preview.error} />;
  if (!preview.data) return null;
  return <GroceryMoveView lists={preview.data.lists} onMoved={() => preview.reload()} />;
}

function itemCount(n: number) {
  return n === 1 ? "1 item" : `${n} items`;
}

function movedLine(r: GroceryMoveResult) {
  const gone = r.lists.filter((l) => l.removed).map((l) => l.name);
  const kept = r.lists.filter((l) => !l.removed && l.left > 0);
  const parts = [`Moved ${itemCount(r.moved)} to PantryOS.`];
  if (gone.length > 0) parts.push(`The old ${gone.join(" and ")} list is gone.`);
  for (const l of kept) parts.push(`${l.name} still has ${itemCount(l.left)} added while this ran; move again to take them too.`);
  return parts.join(" ");
}

// Separated from its fetch so the render check can show it with lists.
export function GroceryMoveView({ lists, onMoved }: { lists: GroceryMoveList[]; onMoved: () => void }) {
  const resuming = isPendingStepUp("POST", PANTRY_GROCERY_MOVE_PATH);
  const move = useAction();
  const [done, setDone] = useState<string | null>(null);
  const total = lists.reduce((n, l) => n + l.items.length, 0);

  if (total === 0) {
    return done ? <div className="note" style={{ marginTop: "var(--space-4)" }}>{done}</div> : null;
  }
  return (
    <>
      <div className="sect" style={{ marginTop: "var(--space-6)" }}>
        <span className="eyebrow">Old grocery list</span>
        <StatusPill kind="warn">{itemCount(total)} to move</StatusPill>
        <span className="fade" />
      </div>
      <div style={{ display: "grid", gap: "var(--space-3)" }}>
        {resuming && <div className="callout">Password confirmed. Move now to finish.</div>}
        <div className="note">
          PantryOS keeps the grocery list now, so requests no longer reach the Wildstone list below. Move what is
          still on it; each item leaves only once PantryOS has it, and the old list goes when it is empty.
        </div>
        {lists.map((l) => (
          <div key={l.name}>
            <strong>{l.name}</strong>
            <div className="muted">{l.items.join(", ")}</div>
          </div>
        ))}
        <div className="row">
          <button
            className="btn btn-primary"
            disabled={move.state.phase === "busy"}
            onClick={async () => {
              await move.run(async () => {
                const r = await api.moveGroceries();
                setDone(movedLine(r));
              }, "moved");
              // Either way the old list changed or may have: a move that
              // stopped part-way took what PantryOS accepted off it.
              onMoved();
            }}
          >
            {move.state.phase === "busy" ? "Moving…" : `Move ${itemCount(total)} to PantryOS`}
          </button>
        </div>
        {move.state.phase === "error" && <ErrorLine error={move.state.message} />}
        {done && <div className="note">{done}</div>}
      </div>
    </>
  );
}
