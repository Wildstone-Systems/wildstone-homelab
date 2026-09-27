import { useState } from "react";
import {
  api,
  isPendingStepUp,
  PANTRY_PAIRING_PATH,
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
  return <PantryPairingView status={status.data} onChanged={() => status.reload()} />;
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
          <div>
            <label htmlFor="pantry-address">PantryOS address</label>
            <input
              id="pantry-address"
              value={draft.address}
              placeholder="e.g. https://pantry.example.internal"
              onChange={(e) => setDraft((d) => ({ ...d, address: e.target.value }))}
            />
          </div>
          <div>
            <label htmlFor="pantry-code">Pairing code</label>
            <input
              id="pantry-code"
              value={draft.code}
              placeholder="ABCD-EFGH"
              autoComplete="off"
              onChange={(e) => setDraft((d) => ({ ...d, code: e.target.value }))}
            />
          </div>
          <div>
            <label htmlFor="pantry-keystone">This Wildstone's address, as PantryOS reaches it</label>
            <input
              id="pantry-keystone"
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
