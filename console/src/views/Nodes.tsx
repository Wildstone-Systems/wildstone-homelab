import { useState } from "react";
import { api, JoinTokenResponse } from "../api";
import { ErrorLine } from "../lib";

// Nodes (ADR-W016 A2 / ADR-W014 A3): pairing — mint a join token and
// hand the operator the ready-to-run install command. First slice of
// the §7.2 onboarding flow; probes and the Restore branch are U2.
export function Nodes() {
  const [minted, setMinted] = useState<JoinTokenResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const mint = () => {
    setBusy(true);
    setError(null);
    api
      .mintJoinToken()
      .then(setMinted)
      .catch((e: Error) => setError(e.message))
      .finally(() => setBusy(false));
  };

  return (
    <>
      <h2>Nodes</h2>
      <div className="sub">
        Pair a new machine: mint a single-use join token, then run the install command on it as
        root. The token below is shown exactly once.
      </div>
      <div className="row">
        <button className="action primary" disabled={busy} onClick={mint}>
          Mint join token
        </button>
      </div>
      <ErrorLine error={error} />
      {minted && (
        <div className="card">
          <h3>Pairing code (valid {Math.round(minted.expiresInSec / 60)} min)</h3>
          <pre>{minted.install}</pre>
          {minted.caFingerprint && (
            <div className="muted">CA fingerprint (pins the join): {minted.caFingerprint}</div>
          )}
        </div>
      )}
    </>
  );
}
