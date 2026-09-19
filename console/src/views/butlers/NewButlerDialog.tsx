import { useState } from "react";
import { X } from "@phosphor-icons/react";
import { api, type ButlerSurface, type Model, type OfferedModel } from "../../api";
import { ErrorLine, useLoad } from "../../lib";
import { Dot } from "../../ui/status";
import { ModelList } from "./TabModel";
import { Say } from "./ButlerDrawer";

interface Draft {
  name: string;
  directive: string;
  surface: ButlerSurface;
  model: string;
}

// Making a Butler.
//
// It is one dialog rather than a wizard because there are four
// decisions and three of them have obvious defaults. What it will NOT
// do is let somebody make a Butler without seeing where its words will
// go: the summary card at the bottom restates the whole choice as a
// sentence, and turns amber the moment a cloud route is selected.
//
// Reach is deliberately absent. A new Butler nobody can talk to is a
// safe thing to have made by accident; a new Butler three people can
// already message is not, so access is granted afterwards, on purpose,
// on its own tab.
export function NewButlerDialog({
  routes,
  existing,
  say,
  onClose,
  onCreated,
  onOpenProviders,
}: {
  routes: Model[];
  existing: string[];
  say: Say;
  onClose: () => void;
  onCreated: (name: string, tab?: string) => void;
  onOpenProviders?: () => void;
}) {
  // The endpoints this house knows about; the models come from
  // whichever is picked, not from what has been configured here.
  const endpoints = Array.from(new Set(routes.map((r) => r.endpoint).filter(Boolean) as string[]));
  const [endpoint, setEndpoint] = useState(endpoints[0] ?? "");
  const provider = routes.find((r) => r.endpoint === endpoint)?.provider ?? "";
  const offered = useLoad(
    () => (endpoint ? api.offeredModels(endpoint) : Promise.resolve(null)),
    [endpoint],
  );
  const [draft, setDraft] = useState<Draft>({
    name: "",
    directive: "",
    surface: "family",
    model: "",
  });
  const [template, setTemplate] = useState("blank");
  const [nameError, setNameError] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const chosen = (offered.data?.models ?? []).find((m) => m.name === draft.model);
  const cloud = chosen ? !chosen.local : false;

  function applyTemplate(key: string) {
    setTemplate(key);
    setNameError(null);
    // A template preselects a model that stays in the house. It is a
    // starting point, not a decision: the person still sees both lists
    // and the summary card restates whichever they end on.
    const local = (offered.data?.models ?? []).find((m) => m.local)?.name ?? draft.model;
    const T: Record<string, Partial<Draft>> = {
      blank: { name: "", directive: "", surface: "family" },
      health: { name: "health", directive: "keeps track of training and sleep", surface: "family" },
      kitchen: { name: "kitchen", directive: "timers, recipes, what's in the freezer", surface: "family" },
      operator: { name: "ops", directive: "diagnoses services, drafts plans", surface: "admin" },
    };
    setDraft((d) => ({ ...d, model: local, ...T[key] } as Draft));
  }

  async function create(openReach: boolean) {
    const name = draft.name.trim();
    if (!name) {
      setNameError("Give it a name — this is what people will say to reach it.");
      return;
    }
    if (existing.some((e) => e.toLowerCase() === name.toLowerCase())) {
      setNameError(`There is already a Butler called ${name}.`);
      return;
    }
    setBusy(true);
    setError(null);
    try {
      await api.createButler({
        name,
        directive: draft.directive.trim(),
        surface: draft.surface,
        profile: draft.surface === "admin" ? "operator" : "household",
        providerModel: draft.model,
        provider,
        endpoint,
      });
      say(`${name} made — give it an address so somebody can talk to it`, "warn");
      onCreated(name, openReach ? "reach" : undefined);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="modal-scrim modal-scroll" onClick={onClose}>
      <div
        className="modal"
        role="dialog"
        aria-modal="true"
        aria-label="New Butler"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="modal-head">
          <h3>New Butler</h3>
          <span className="muted">One job each. Reach and tools can wait.</span>
          <button type="button" className="btn btn-ghost drawer-close" onClick={onClose} aria-label="Close">
            <X size={16} />
          </button>
        </div>

        <div className="modal-body tab-stack">
          <div>
            <div className="eyebrow">START FROM</div>
            <div className="chips">
              {[
                { key: "blank", label: "Blank" },
                { key: "health", label: "Health" },
                { key: "kitchen", label: "Kitchen" },
                { key: "operator", label: "Operator" },
              ].map((t) => (
                <button
                  key={t.key}
                  type="button"
                  aria-pressed={template === t.key}
                  className={template === t.key ? "chip on" : "chip"}
                  onClick={() => applyTemplate(t.key)}
                >
                  {t.label}
                </button>
              ))}
            </div>
          </div>

          <div>
            <label htmlFor="new-butler-name">Name</label>
            <input
              id="new-butler-name"
              value={draft.name}
              placeholder="e.g. Alfred, or health"
              onChange={(e) => {
                setDraft({ ...draft, name: e.target.value });
                setNameError(null);
              }}
            />
            {nameError && <div className="error">{nameError}</div>}
          </div>

          <div>
            <label htmlFor="new-butler-directive">What it is for</label>
            <textarea
              id="new-butler-directive"
              rows={3}
              value={draft.directive}
              placeholder="e.g. keeps track of training and sleep"
              onChange={(e) => setDraft({ ...draft, directive: e.target.value })}
            />
            <div className="note">Shapes how it talks. It grants nothing.</div>
          </div>

          <div>
            <div className="eyebrow">SURFACE</div>
            <div className="surface-cards">
              {[
                { key: "family" as const, title: "Household", blurb: "Reads the home. Cannot touch the home lab." },
                { key: "admin" as const, title: "Operator", blurb: "May propose home lab changes. Never approves." },
              ].map((o) => (
                <button
                  key={o.key}
                  type="button"
                  aria-pressed={draft.surface === o.key}
                  className={`surface-card${draft.surface === o.key ? " on" : ""}`}
                  onClick={() => setDraft({ ...draft, surface: o.key })}
                >
                  <strong>{o.title}</strong>
                  <span className="muted">{o.blurb}</span>
                </button>
              ))}
            </div>
            <div className="note">
              A surface is the ceiling its tools are granted against, and it cannot be changed
              afterwards.
            </div>
          </div>

          <div>
            {endpoints.length > 1 && (
              <select
                aria-label="Provider endpoint"
                value={endpoint}
                onChange={(e) => setEndpoint(e.target.value)}
              >
                {endpoints.map((e) => (
                  <option key={e} value={e}>
                    {e}
                  </option>
                ))}
              </select>
            )}
            {endpoints.length === 0 && (
              <div className="muted">
                No model provider yet, so there is no model to choose.{" "}
                {onOpenProviders && (
                  <button className="btn btn-small" onClick={onOpenProviders}>
                    Add a model provider
                  </button>
                )}
              </div>
            )}
            <ErrorLine error={offered.error} />
            {offered.loading && <div className="muted">Asking {endpoint} what it serves…</div>}
            <ModelList
              title="ON YOUR HARDWARE"
              models={(offered.data?.models ?? []).filter((m) => m.local)}
              provider={provider}
              current={draft.model}
              busy={busy}
              onPick={(m: OfferedModel) => setDraft({ ...draft, model: m.name })}
            />
            <ModelList
              title="OUTSIDE THE HOUSE"
              models={(offered.data?.models ?? []).filter((m) => !m.local)}
              provider={provider}
              current={draft.model}
              busy={busy}
              onPick={(m: OfferedModel) => setDraft({ ...draft, model: m.name })}
            />
          </div>

          <div className={`card summary-card${cloud ? " summary-cloud" : ""}`}>
            <div className="eyebrow">YOU ARE MAKING</div>
            <div>
              {cloud && <Dot kind="warn" />} a {draft.surface === "admin" ? "operator" : "household"}{" "}
              Butler called {draft.name.trim() || "—"} on {draft.model || "no model"}, which{" "}
              {cloud ? "sends what it is told outside the house" : "stays in the house"}. Nobody can
              talk to it until you give it an address.
            </div>
          </div>

          <ErrorLine error={error} />
        </div>

        <div className="modal-foot">
          <button className="btn btn-primary" disabled={busy || !draft.model} onClick={() => void create(false)}>
            {busy ? "Making…" : "Create"}
          </button>
          <button className="btn btn-secondary" disabled={busy || !draft.model} onClick={() => void create(true)}>
            Create and open Reach
          </button>
          <button className="btn btn-ghost drawer-foot-end" disabled={busy} onClick={onClose}>
            Cancel
          </button>
        </div>
      </div>
    </div>
  );
}
