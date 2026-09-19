import { useState } from "react";
import { X } from "@phosphor-icons/react";
import { api, type Model, type OfferedModel } from "../../api";
import { ErrorLine } from "../../lib";
import { Dot, type Kind } from "../../ui/status";

// ProvidersDrawer is where the house says which model servers it has.
//
// A Butler thinks with one model, and a model comes from a provider — a
// server at an address. Keystone keeps no separate provider record: a
// provider is the server behind the models registered from it, and every
// screen that offers a model (New Butler, a Butler's Model tab) lists
// the servers those registrations name. So a fresh install, with none
// registered, offered nothing, and nothing anywhere took an address: the
// screen that used to (ButlerSettings) stopped being shown when models
// became a property of each Butler (ADR-W031). The dashboard's "give the
// assistant a model" led here and dead-ended.
//
// This drawer lists the servers the house knows and adds one: its
// address, what it serves, and which of those to register. A typed
// server is spoken to as Ollama, which is what listing its models asks
// (/api/tags). A hosted provider's key is never typed here — it goes in
// Keystone's environment (ADR-W017 A4).
export const TYPED_PROVIDER = "ollama";

type Provider = { endpoint: string; provider: string; models: Model[] };

function providersOf(models: Model[]): Provider[] {
  const by = new Map<string, Provider>();
  for (const m of models) {
    const endpoint = m.endpoint ?? "";
    if (!endpoint) continue;
    const key = `${m.provider} ${endpoint}`;
    if (!by.has(key)) by.set(key, { endpoint, provider: m.provider, models: [] });
    by.get(key)!.models.push(m);
  }
  return [...by.values()];
}

function readinessKind(r: Model["readiness"]): Kind {
  return r === "ready" ? "ok" : r === "unavailable" ? "bad" : "warn";
}

export function ProvidersDrawer({
  models,
  onClose,
  onChanged,
}: {
  models: Model[];
  onClose: () => void;
  onChanged: () => void;
}) {
  const providers = providersOf(models);
  const [adding, setAdding] = useState(providers.length === 0);
  const [address, setAddress] = useState("");
  const [endpoint, setEndpoint] = useState("");
  const [offered, setOffered] = useState<OfferedModel[] | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const registered = new Set(models.map((m) => `${m.endpoint} ${m.providerModel ?? m.name}`));

  const connect = async () => {
    const typed = address.trim().replace(/\/+$/, "");
    if (!typed) return;
    const e = /^https?:\/\//.test(typed) ? typed : `http://${typed}`;
    setBusy("connect");
    setError(null);
    setOffered(null);
    try {
      const res = await api.offeredModels(e);
      setEndpoint(res.endpoint || e);
      setOffered(res.models ?? []);
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(null);
    }
  };

  const add = async (m: OfferedModel) => {
    setBusy(m.name);
    setError(null);
    try {
      // Named as the provider names it, exactly as assigning a model
      // from New Butler registers one — so the two paths agree.
      await api.upsertModel({
        name: m.name,
        kind: m.local ? "local" : "cloud",
        provider: TYPED_PROVIDER,
        model: m.name,
        endpoint,
        enabled: true,
      });
      onChanged();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(null);
    }
  };

  return (
    <div className="drawer-scrim" onClick={onClose}>
      <aside
        className="drawer drawer-wide"
        role="dialog"
        aria-modal="true"
        aria-label="Model providers"
        onClick={(e) => e.stopPropagation()}
      >
        <button className="btn btn-ghost btn-small drawer-close" onClick={onClose} aria-label="Close">
          <X size={15} />
        </button>
        <div className="drawer-head">
          <h3>Model providers</h3>
        </div>
        <div className="muted">
          The servers your Butlers think with. Add yours here, then give a Butler one of its models.
        </div>

        <div className="drawer-body stack" style={{ gap: "var(--space-4)", marginTop: "var(--space-4)" }}>
          {providers.length === 0 && !adding && (
            <div className="muted">No model provider yet.</div>
          )}
          {providers.map((p) => (
            <div key={`${p.provider} ${p.endpoint}`} className="card">
              <div className="row" style={{ display: "flex", gap: "var(--space-2)", alignItems: "center" }}>
                <strong>{p.endpoint}</strong>
                <span className="chip">{p.provider}</span>
              </div>
              <div className="stack" style={{ marginTop: "var(--space-2)" }}>
                {p.models.map((m) => (
                  <div key={m.name} className="row" style={{ display: "flex", gap: "var(--space-2)", alignItems: "center" }}>
                    <Dot kind={readinessKind(m.readiness)} />
                    <span>{m.providerModel ?? m.name}</span>
                    <span className="muted">
                      {m.kind === "cloud" ? "outside the house" : "on your hardware"} · {m.readiness}
                      {m.enabled ? "" : " · turned off"}
                    </span>
                  </div>
                ))}
              </div>
            </div>
          ))}

          {!adding && (
            <div>
              <button className="btn" onClick={() => setAdding(true)}>
                Add a provider
              </button>
            </div>
          )}

          {adding && (
            <div className="card">
              <div className="eyebrow">ADD A PROVIDER</div>
              <div className="muted">
                The address of an Ollama server — on this machine or another one in the house.
              </div>
              <div className="row" style={{ display: "flex", gap: "var(--space-2)", marginTop: "var(--space-2)" }}>
                <input
                  aria-label="Provider address"
                  value={address}
                  onChange={(e) => setAddress(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter") {
                      e.preventDefault();
                      connect();
                    }
                  }}
                  placeholder="http://192.168.1.20:11434"
                />
                <button className="btn btn-primary" onClick={connect} disabled={!address.trim() || busy !== null}>
                  {busy === "connect" ? "Connecting…" : "Connect"}
                </button>
              </div>
              <ErrorLine error={error} />
              {offered && offered.length === 0 && (
                <div className="muted">{endpoint} answered, but serves no models yet.</div>
              )}
              {offered && offered.length > 0 && (
                <div className="stack" style={{ marginTop: "var(--space-3)" }}>
                  <div className="eyebrow">WHAT {endpoint} SERVES</div>
                  {offered.map((m) => {
                    const have = registered.has(`${endpoint} ${m.name}`);
                    return (
                      <div key={m.name} className="row" style={{ display: "flex", gap: "var(--space-2)", alignItems: "center" }}>
                        <Dot kind={m.local ? "ok" : "warn"} />
                        <span>{m.name}</span>
                        <span className="muted">
                          {m.local ? "on your hardware" : "outside the house"}
                          {m.parameters ? ` · ${m.parameters}` : ""}
                        </span>
                        <button
                          className="btn btn-small"
                          style={{ marginLeft: "auto" }}
                          disabled={have || busy !== null}
                          onClick={() => add(m)}
                        >
                          {have ? "Added" : busy === m.name ? "Adding…" : "Add"}
                        </button>
                      </div>
                    );
                  })}
                </div>
              )}
            </div>
          )}
        </div>
      </aside>
    </div>
  );
}
