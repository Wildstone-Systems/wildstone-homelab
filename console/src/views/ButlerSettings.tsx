import { useState } from "react";
import { api, OfferedModel } from "../api";
import { ErrorLine, useLoad } from "../lib";

// Choosing which model answers (ADR-W017 A9).
//
// The provider, model and endpoint used to be startup flags, so changing
// them meant editing a systemd environment file and restarting the
// control plane — which drops agent sessions and proposes a drift repair
// that reports healthy resources as absent. Changing which model answers
// a question should not make a household look broken.
//
// The model list matters as much as the field. Nothing could say what an
// endpoint offered, so the name was typed from memory and a typo
// produced a Butler that configured cleanly and failed on first use.
const PROVIDERS = [
  { id: "ollama", label: "Ollama — a model running on your own hardware" },
  { id: "anthropic", label: "Anthropic — needs ANTHROPIC_API_KEY in keystone's environment" },
  { id: "openai", label: "OpenAI-compatible — needs OPENAI_API_KEY in keystone's environment" },
  { id: "off", label: "Off — no Butler" },
];

function size(m: OfferedModel): string {
  if (!m.sizeBytes || m.sizeBytes < 1 << 20) return "";
  return `${(m.sizeBytes / 1e9).toFixed(1)} GB`;
}

export function ButlerSettings({ onSaved }: { onSaved?: () => void }) {
  const status = useLoad(() => api.butlerConfig());
  const [provider, setProvider] = useState("");
  const [endpoint, setEndpoint] = useState("");
  const [model, setModel] = useState("");
  const [models, setModels] = useState<OfferedModel[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [saved, setSaved] = useState<string | null>(null);

  const cfg = status.data?.config;
  const p = provider || cfg?.provider || "ollama";
  const e = endpoint || cfg?.endpoint || "";
  const m = model || cfg?.model || "";

  async function loadModels() {
    setBusy("models");
    setError(null);
    try {
      setModels((await api.offeredModels(e || undefined)).models);
    } catch (err) {
      setModels(null);
      setError(String(err));
    } finally {
      setBusy(null);
    }
  }

  async function save() {
    setBusy("save");
    setError(null);
    setSaved(null);
    try {
      const r = await api.setButlerConfig({ provider: p, model: m, endpoint: e });
      setSaved(
        r.config.probeMode === "full"
          ? "Saved. The model passed its tool check, so the Butler can propose changes."
          : `Saved, but running read-only: ${r.config.probeDetail ?? "the model did not pass its tool check"}`,
      );
      status.reload();
      // The parent screen decides whether Ask is usable from its own
      // status call, so saving here has to tell it. Without this,
      // enabling the Butler left Ask disabled and turning it off left
      // Ask enabled, until the page was reloaded.
      onSaved?.();
    } catch (err) {
      setError(String(err));
    } finally {
      setBusy(null);
    }
  }

  return (
    <>
      <h3>Butler settings</h3>
      <div className="sub">
        Which model answers questions about your household. Changes apply immediately —
        Keystone is not restarted.
      </div>
      <ErrorLine error={status.error ?? error} />

      <div className="row">
        <label>provider</label>
        <select value={p} onChange={(ev) => setProvider(ev.target.value)}>
          {PROVIDERS.map((o) => (
            <option key={o.id} value={o.id}>
              {o.label}
            </option>
          ))}
        </select>
      </div>

      {p !== "off" && (
        <>
          <div className="row">
            <label>
              endpoint
              <div className="muted">Where the models run, e.g. http://192.168.1.99:11434</div>
            </label>
            <input value={e} onChange={(ev) => setEndpoint(ev.target.value)} placeholder="http://host:11434" />
            <button onClick={loadModels} disabled={busy !== null}>
              {busy === "models" ? "asking…" : "List models"}
            </button>
          </div>

          <div className="row">
            <label>model</label>
            {models ? (
              <select value={m} onChange={(ev) => setModel(ev.target.value)}>
                <option value="">choose…</option>
                {models.map((mo) => (
                  <option key={mo.name} value={mo.name}>
                    {mo.name}
                    {mo.parameters ? ` · ${mo.parameters}` : ""}
                    {size(mo) ? ` · ${size(mo)}` : ""}
                    {mo.local ? "" : " · runs off-site"}
                  </option>
                ))}
              </select>
            ) : (
              <input value={m} onChange={(ev) => setModel(ev.target.value)} placeholder="list models to choose one" />
            )}
          </div>

          {models && (
            <div className="sub">
              {models.filter((x) => x.local).length} on your hardware,{" "}
              {models.filter((x) => !x.local).length} proxied off-site. A model marked{" "}
              <em>runs off-site</em> sends your household's questions to someone else's machine.
            </div>
          )}
        </>
      )}

      <div className="row">
        <button className="btn btn-secondary" onClick={save} disabled={busy !== null || (p !== "off" && !m)}>
          {busy === "save" ? "applying…" : "Save and apply"}
        </button>
      </div>

      {saved && <div className="card">{saved}</div>}

      {cfg && (
        <div className="card">
          <h3>Now running</h3>
          <div className="chips">
            <span className="chip">{cfg.provider}</span>
            {cfg.model && <span className="chip">{cfg.model}</span>}
            {cfg.endpoint && <span className="chip">{cfg.endpoint}</span>}
            {cfg.probeMode && <span className="chip">{cfg.probeMode}</span>}
          </div>
          {cfg.updatedBy && (
            <div className="muted">
              last changed by {cfg.updatedBy}
              {cfg.updatedAt ? ` on ${new Date(cfg.updatedAt).toLocaleString()}` : ""}
            </div>
          )}
        </div>
      )}
    </>
  );
}
