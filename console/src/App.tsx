import { useEffect, useState } from "react";
import { api, getToken, Identity, setOnUnauthorized, setToken } from "./api";
import { ErrorLine } from "./lib";
import { Backups } from "./views/Backups";
import { Butler } from "./views/Butler";
import { Events } from "./views/Events";
import { Home } from "./views/Home";
import { IdentityView } from "./views/Identity";
import { Nodes } from "./views/Nodes";
import { Plans } from "./views/Plans";
import { Secrets } from "./views/Secrets";
import { Services } from "./views/Services";

type View =
  | "home"
  | "services"
  | "butler"
  | "plans"
  | "events"
  | "backups"
  | "nodes"
  | "secrets"
  | "identity";

const VIEWS: { id: View; label: string }[] = [
  { id: "home", label: "Home" },
  { id: "services", label: "Services" },
  { id: "butler", label: "Butler" },
  { id: "plans", label: "Plans" },
  { id: "events", label: "Events" },
  { id: "backups", label: "Backups" },
  { id: "nodes", label: "Nodes" },
  { id: "secrets", label: "Secrets" },
  { id: "identity", label: "Identity" },
];

export function App() {
  const [authed, setAuthed] = useState(getToken() !== null);
  const [who, setWho] = useState<Identity | null>(null);
  const [view, setView] = useState<View>("home");

  useEffect(() => {
    setOnUnauthorized(() => setAuthed(false));
  }, []);
  useEffect(() => {
    if (authed) {
      api.whoami().then(setWho).catch(() => {});
    }
  }, [authed]);

  if (!authed) {
    return <TokenGate onDone={() => setAuthed(true)} />;
  }

  return (
    <div className="shell">
      <nav className="nav">
        <h1>Wildstone</h1>
        {VIEWS.map((v) => (
          <button
            key={v.id}
            className={view === v.id ? "active" : ""}
            onClick={() => setView(v.id)}
          >
            {v.label}
          </button>
        ))}
        {who && (
          <div className="who">
            {who.name} ({who.role})
          </div>
        )}
      </nav>
      <main className="main">
        {view === "home" && <Home />}
        {view === "services" && <Services />}
        {view === "butler" && <Butler />}
        {view === "plans" && <Plans />}
        {view === "events" && <Events />}
        {view === "backups" && <Backups />}
        {view === "nodes" && <Nodes />}
        {view === "secrets" && <Secrets />}
        {view === "identity" && <IdentityView />}
      </main>
    </div>
  );
}

// TokenGate (ADR-W016 A3): paste an API token; verified with whoami
// before it is kept, and kept only in sessionStorage.
function TokenGate({ onDone }: { onDone: () => void }) {
  const [input, setInput] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = () => {
    setBusy(true);
    setError(null);
    setToken(input.trim());
    api
      .whoami()
      .then(() => onDone())
      .catch((e: Error) => setError(e.message))
      .finally(() => setBusy(false));
  };

  return (
    <div className="gate">
      <div className="card">
        <h3>Wildstone Console</h3>
        <div className="muted" style={{ marginBottom: 8 }}>
          Paste an API token (first start printed one; mint more with wild).
        </div>
        <div className="row">
          <input
            type="password"
            placeholder="wst_…"
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && input && submit()}
          />
        </div>
        <button className="action primary" disabled={busy || !input} onClick={submit}>
          Enter
        </button>
        <ErrorLine error={error} />
      </div>
    </div>
  );
}
