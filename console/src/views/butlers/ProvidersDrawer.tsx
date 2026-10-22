import { useEffect, useMemo, useState } from "react";
import { X } from "@phosphor-icons/react";
import { api, type Model, type OfferedModel, type ProviderKind } from "../../api";
import { ErrorLine, useLoad } from "../../lib";
import { Dot, type Kind } from "../../ui/status";
import { SecretInput } from "../../ui/SecretInput";

// ProvidersDrawer is where the house says which model servers it has,
// and which of their models its Butlers may use.
//
// A Butler thinks with one model, and a model comes from a provider — a
// server at an address. Keystone keeps no separate provider record: a
// provider is the server behind the models registered from it, and New
// Butler and a Butler's Model tab offer exactly those registered models.
// So this drawer is the one place that decides what is available: add a
// provider, pick which of its models to register, and come back later to
// add or remove some (Manage models).
//
// Which kinds exist, and whether each hosted one has its key, comes from
// the server's catalog (GET /butler/providers) — the same list the
// factory builds from. A hosted provider's key is entered here, write-
// only, or set in keystone's environment, which wins (ADR-W017 A10).

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

function readinessKind(m: Model): Kind {
  if (!m.enabled) return "warn";
  return m.readiness === "ready" ? "ok" : m.readiness === "unavailable" ? "bad" : "warn";
}

function fallbackKind(name: string): ProviderKind {
  return { name, label: name, keyRequired: false, hosted: false, client: name, keySet: true };
}

// KeyControl is a provider's key: whether keystone has one and from
// where, and — unless it comes from keystone's environment, which wins
// and is changed there — a write-only field to enter or replace it
// (ADR-W017 A10). The key is sent once and the field cleared; nothing
// ever shows it again.
function KeyControl({ kind, onChanged }: { kind: ProviderKind; onChanged: () => void }) {
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  if (!kind.keyEnv) return null;

  const save = async () => {
    setBusy(true);
    setError(null);
    try {
      await api.setProviderKey(kind.name, value.trim());
      setValue("");
      setEditing(false);
      onChanged();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };
  const remove = async () => {
    setBusy(true);
    setError(null);
    try {
      await api.removeProviderKey(kind.name);
      onChanged();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  if (kind.keySource === "environment") {
    return (
      <div className="muted">
        <Dot kind="ok" /> Key set in keystone&apos;s environment (<code>{kind.keyEnv}</code>) — change it there.
      </div>
    );
  }
  if (kind.keySet && !editing) {
    return (
      <div className="row" style={{ display: "flex", gap: "var(--space-2)", alignItems: "center" }}>
        <span className="muted">
          <Dot kind="ok" /> Key set here. It is never shown again.
        </span>
        <button className="btn btn-ghost btn-small" onClick={() => setEditing(true)} disabled={busy}>
          Replace
        </button>
        <button className="btn btn-ghost btn-small" onClick={remove} disabled={busy}>
          Remove
        </button>
        <ErrorLine error={error} />
      </div>
    );
  }
  return (
    <div className="stack" style={{ gap: "var(--space-1)" }}>
      <div className="row" style={{ display: "flex", gap: "var(--space-2)" }}>
        <SecretInput
          autoComplete="off"
          aria-label={`${kind.label} API key`}
          value={value}
          onChange={(e) => setValue(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && value.trim()) {
              e.preventDefault();
              save();
            }
          }}
          placeholder={`${kind.label} API key`}
        />
        <button className="btn btn-primary btn-small" onClick={save} disabled={busy || !value.trim()}>
          {busy ? "Saving…" : "Save key"}
        </button>
        {editing && (
          <button className="btn btn-ghost btn-small" onClick={() => setEditing(false)} disabled={busy}>
            Cancel
          </button>
        )}
      </div>
      <div className="muted">
        {kind.keyRequired ? "Needed to connect. " : "Optional. "}Kept on the Wildstone machine, readable only by
        root, and never shown again. (Or set <code>{kind.keyEnv}</code> in keystone&apos;s environment.)
      </div>
      <ErrorLine error={error} />
    </div>
  );
}

// ModelPicker lists what one provider serves, with the registered ones
// ticked, and applies the difference: ticked models are registered (or
// turned back on), unticked ones removed — or turned off, when a Butler
// is using one and the server refuses to remove it.
function ModelPicker({
  kind,
  endpoint,
  registered,
  onChanged,
  onDone,
}: {
  kind: ProviderKind;
  endpoint: string;
  registered: Model[];
  onChanged: () => void;
  onDone?: () => void;
}) {
  const offered = useLoad(() => api.offeredModels(endpoint, kind.name), [endpoint, kind.name]);
  const have = useMemo(() => new Map(registered.map((m) => [m.providerModel ?? m.name, m])), [registered]);
  const [ticked, setTicked] = useState<Set<string>>(
    () => new Set(registered.filter((m) => m.enabled).map((m) => m.providerModel ?? m.name)),
  );
  const [filter, setFilter] = useState("");
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const served: OfferedModel[] = offered.data?.models ?? [];
  // A registered model the server no longer lists still shows, so it can
  // be removed.
  const rows: OfferedModel[] = [
    ...served,
    ...registered
      .filter((m) => !served.some((o) => o.name === (m.providerModel ?? m.name)))
      .map((m) => ({ name: m.providerModel ?? m.name, local: m.kind === "local" })),
  ];
  const shown = filter ? rows.filter((m) => m.name.toLowerCase().includes(filter.toLowerCase())) : rows;
  const toAdd = rows.filter((m) => ticked.has(m.name) && (!have.has(m.name) || !have.get(m.name)!.enabled));
  const toRemove = [...have.values()].filter((m) => m.enabled && !ticked.has(m.providerModel ?? m.name));
  const allShownTicked = shown.length > 0 && shown.every((m) => ticked.has(m.name));
  const cloud = (m: OfferedModel) => kind.hosted || !m.local;

  const toggle = (name: string) =>
    setTicked((prev) => {
      const next = new Set(prev);
      if (next.has(name)) next.delete(name);
      else next.add(name);
      return next;
    });

  const apply = async () => {
    setBusy(true);
    setError(null);
    setResult(null);
    const said: string[] = [];
    const failed: string[] = [];
    for (const m of toAdd) {
      try {
        const existing = have.get(m.name);
        if (existing) {
          await api.setModelEnabled(existing.name, true);
        } else {
          // Named as the provider names it, exactly as assigning a model
          // from New Butler registers one — so the two paths agree.
          await api.upsertModel({
            name: m.name,
            kind: cloud(m) ? "cloud" : "local",
            provider: kind.name,
            model: m.name,
            endpoint,
            enabled: true,
          });
        }
      } catch (err) {
        failed.push(`${m.name}: ${(err as Error).message}`);
      }
    }
    for (const m of toRemove) {
      try {
        await api.deleteModel(m.name);
      } catch {
        // A Butler is assigned to it: the server keeps it, so turn it off
        // instead — nobody new can pick it, and its Butler keeps working
        // until it is moved.
        try {
          await api.setModelEnabled(m.name, false);
          said.push(`${m.providerModel ?? m.name} is in use by a Butler, so it was turned off rather than removed`);
        } catch (err) {
          failed.push(`${m.name}: ${(err as Error).message}`);
        }
      }
    }
    setBusy(false);
    if (toRemove.length) said.unshift(`Removed ${toRemove.length}`);
    if (toAdd.length) said.unshift(`Added ${toAdd.length}`);
    setResult(said.join(". ") || null);
    if (failed.length) setError(`Not done — ${failed.join("; ")}`);
    onChanged();
  };

  return (
    <div style={{ marginTop: "var(--space-3)" }}>
      {offered.loading && <div className="muted">Asking {endpoint} what it serves…</div>}
      <ErrorLine error={offered.error} />
      {!offered.loading && rows.length === 0 && !offered.error && (
        <div className="muted">{endpoint} answered, but serves no models yet.</div>
      )}
      {rows.length > 0 && (
        <>
          {rows.length > 12 && (
            <input
              aria-label="Filter models"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              placeholder={`Filter ${rows.length} models`}
              style={{ marginBottom: "var(--space-2)" }}
            />
          )}
          <div className="model-rows-head">
            <label className="model-pick">
              <input
                type="checkbox"
                checked={allShownTicked}
                disabled={busy}
                onChange={() =>
                  setTicked((prev) => {
                    const next = new Set(prev);
                    for (const m of shown) {
                      if (allShownTicked) next.delete(m.name);
                      else next.add(m.name);
                    }
                    return next;
                  })
                }
              />
              <span className="eyebrow">WHAT {endpoint} SERVES</span>
            </label>
            <span className="muted">{ticked.size} available to Butlers</span>
          </div>
          <div className="model-rows model-rows-scroll">
            {shown.map((m) => {
              const reg = have.get(m.name);
              const listed = served.some((o) => o.name === m.name);
              return (
                <label key={m.name} className="model-row model-pick">
                  <input
                    type="checkbox"
                    checked={ticked.has(m.name)}
                    disabled={busy}
                    onChange={() => toggle(m.name)}
                    aria-label={`Make ${m.name} available`}
                  />
                  <Dot kind={cloud(m) ? "warn" : "ok"} />
                  <strong>{m.name}</strong>
                  <span className="muted">
                    {cloud(m) ? "outside the house" : "on your hardware"}
                    {m.parameters ? ` · ${m.parameters}` : ""}
                    {!listed && !offered.loading ? " · no longer served" : ""}
                  </span>
                  {reg && <span className="chip model-row-end">{reg.enabled ? "Added" : "Off"}</span>}
                </label>
              );
            })}
          </div>
        </>
      )}
      <ErrorLine error={error} />
      {result && <div className="muted">{result}.</div>}
      <div
        className="row"
        style={{ display: "flex", gap: "var(--space-2)", justifyContent: "flex-end", marginTop: "var(--space-3)" }}
      >
        {onDone && (
          <button className="btn btn-ghost" onClick={onDone} disabled={busy}>
            Done
          </button>
        )}
        <button
          className="btn btn-primary"
          onClick={apply}
          disabled={busy || (toAdd.length === 0 && toRemove.length === 0)}
        >
          {busy ? "Applying…" : `Apply (${toAdd.length} to add, ${toRemove.length} to remove)`}
        </button>
      </div>
    </div>
  );
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
  const kinds = useLoad(() => api.providerKinds());
  const kindList = kinds.data?.providers ?? [];
  const kindOf = (name: string) => kindList.find((k) => k.name === name) ?? fallbackKind(name);
  const providers = providersOf(models);

  const [managing, setManaging] = useState<string | null>(null);
  const [adding, setAdding] = useState(providers.length === 0);
  const [kindName, setKindName] = useState("ollama");
  const [address, setAddress] = useState("");
  const [connected, setConnected] = useState<string | null>(null);
  const [checking, setChecking] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const kind = kindOf(kindName);
  // A new kind fills in its known address; the operator can change it.
  useEffect(() => {
    setAddress(kind.defaultEndpoint ?? "");
    setConnected(null);
  }, [kindName, kind.defaultEndpoint]);

  const connect = () => {
    const typed = address.trim().replace(/\/+$/, "");
    if (!typed) return;
    setConnected(/^https?:\/\//.test(typed) ? typed : `http://${typed}`);
  };

  const recheck = async (m: Model) => {
    setChecking(m.name);
    setError(null);
    try {
      await api.probeModel(m.name);
      onChanged();
    } catch (err) {
      setError(`${m.providerModel ?? m.name}: ${(err as Error).message}`);
    } finally {
      setChecking(null);
    }
  };

  const blocked = kind.keyRequired && !kind.keySet;

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
        <div className="muted">The servers your Butlers think with, and which of their models are available to them.</div>
        <ErrorLine error={kinds.error} />
        <ErrorLine error={error} />

        <div className="drawer-body stack" style={{ gap: "var(--space-4)", marginTop: "var(--space-4)" }}>
          {providers.length === 0 && !adding && <div className="muted">No model provider yet.</div>}
          {providers.map((p) => {
            const k = kindOf(p.provider);
            const key = `${p.provider} ${p.endpoint}`;
            return (
              <div key={key} className="card">
                <div className="row" style={{ display: "flex", gap: "var(--space-2)", alignItems: "center" }}>
                  <strong>{p.endpoint}</strong>
                  <span className="chip">{k.label}</span>
                  <button className="btn btn-small model-row-end" onClick={() => setManaging(managing === key ? null : key)}>
                    {managing === key ? "Close" : "Manage models"}
                  </button>
                </div>
                <KeyControl kind={k} onChanged={kinds.reload} />
                {managing === key ? (
                  <ModelPicker
                    kind={k}
                    endpoint={p.endpoint}
                    registered={p.models}
                    onChanged={onChanged}
                    onDone={() => setManaging(null)}
                  />
                ) : (
                  <div className="model-rows" style={{ marginTop: "var(--space-2)" }}>
                    {p.models.map((m) => (
                      <div key={m.name} className="model-row">
                        <Dot kind={readinessKind(m)} />
                        <span>{m.providerModel ?? m.name}</span>
                        <span className="muted" title={m.readinessDetail}>
                          {m.kind === "cloud" ? "outside the house" : "on your hardware"} ·{" "}
                          {m.enabled ? m.readiness : "turned off"}
                        </span>
                        <button
                          className="btn btn-ghost btn-small model-row-end"
                          disabled={checking !== null}
                          onClick={() => recheck(m)}
                          title="Ask the model to call a tool, and record whether it can"
                        >
                          {checking === m.name ? "Checking…" : "Re-check"}
                        </button>
                      </div>
                    ))}
                  </div>
                )}
              </div>
            );
          })}

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
              <div className="stack" style={{ gap: "var(--space-2)", marginTop: "var(--space-2)" }}>
                <select aria-label="Provider type" value={kindName} onChange={(e) => setKindName(e.target.value)}>
                  {(kindList.length ? kindList : [fallbackKind("ollama")]).map((k) => (
                    <option key={k.name} value={k.name}>
                      {k.label}
                    </option>
                  ))}
                </select>
                <KeyControl kind={kind} onChanged={kinds.reload} />
                <div className="row" style={{ display: "flex", gap: "var(--space-2)" }}>
                  <input
                    aria-label="Provider address"
                    value={address}
                    onChange={(e) => {
                      setAddress(e.target.value);
                      setConnected(null);
                    }}
                    onKeyDown={(e) => {
                      if (e.key === "Enter") {
                        e.preventDefault();
                        connect();
                      }
                    }}
                    placeholder={
                      kind.name === "ollama"
                        ? "http://192.168.1.20:11434"
                        : kind.name === "openai-compatible"
                          ? "http://192.168.1.20:1234/v1"
                          : kind.defaultEndpoint
                    }
                  />
                  <button className="btn btn-primary" onClick={connect} disabled={!address.trim() || blocked}>
                    Connect
                  </button>
                </div>
                {connected && (
                  <ModelPicker
                    key={`${kind.name} ${connected}`}
                    kind={kind}
                    endpoint={connected}
                    registered={models.filter((m) => m.endpoint === connected && m.provider === kind.name)}
                    onChanged={onChanged}
                    onDone={() => {
                      setAdding(false);
                      setConnected(null);
                    }}
                  />
                )}
              </div>
            </div>
          )}
        </div>
      </aside>
    </div>
  );
}
