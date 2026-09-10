import { useState } from "react";
import { Warning } from "@phosphor-icons/react";
import { api, modelLabel, type AskResponse, type ButlerRow, type Model, type OfferedModel } from "../../api";
import { ErrorLine, useLoad } from "../../lib";
import { Confirm } from "../../ui/Confirm";
import { Dot, Kind } from "../../ui/status";
import { Say } from "./ButlerDrawer";

// Which model answers, and what that means for the material.
//
// The list is what the PROVIDER actually serves, not what this house
// has configured. Those were the same thing while a deployment had two
// models set up by hand, and the difference showed the moment it did
// not: a tab offering "kitchen-box" and "ollama-cloud" is naming
// places, and the person choosing wants to see gemma4:e4b and
// qwen3.5:9b — the models. A configured entry is created behind this
// when one is chosen for the first time.
//
// Two labelled lists rather than a select, because ON YOUR HARDWARE and
// OUTSIDE THE HOUSE is the most consequential thing about the choice
// and a dropdown renders it as a suffix somebody scrolls past. Whether
// a model is local is the SERVER's verdict, drawn from the provider's
// own listing: Ollama does not say outright, so it is inferred from the
// naming and a size no real weights file could be, and it is
// deliberately wrong in the safe direction — an unrecognised scheme
// reads as leaving the house.
//
// Switching applies at once (A8). The only friction is local-to-cloud,
// which gets a confirmation. Cloud-to-local needs none: it is strictly
// a narrowing.
export function TabModel({
  row,
  configured,
  health,
  say,
  onChanged,
}: {
  row: ButlerRow;
  // Only used to discover which provider endpoints this house knows
  // about. The list a person picks from comes from the provider.
  configured: Model[];
  health: { kind: Kind; long: string };
  say: Say;
  onChanged: () => void;
}) {
  const b = row.butler;
  // The endpoints this house knows about, newest choice first: the one
  // this Butler is on, then any other a configured model names. A
  // household with one Ollama sees one list and never thinks about it.
  const endpoints = Array.from(
    new Set([b.endpoint, ...configured.map((m) => m.endpoint)].filter(Boolean) as string[]),
  );
  const [endpoint, setEndpoint] = useState(endpoints[0] ?? "");
  const offered = useLoad(
    () => (endpoint ? api.offeredModels(endpoint) : Promise.resolve(null)),
    [endpoint],
  );
  // What this Butler answers on, in the provider's own vocabulary —
  // which is what the list below is in. Its configured NAME may be
  // something else entirely.
  const current = b.providerModel ?? "";
  // The provider serving the endpoint being listed. The Butler's own is
  // the right answer while it is on that endpoint; a configured model
  // naming the same endpoint answers for the others.
  const providerOf =
    (endpoint === b.endpoint ? b.provider : undefined) ??
    configured.find((m) => m.endpoint === endpoint)?.provider ??
    "";
  const [busy, setBusy] = useState(false);
  const [probing, setProbing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [warning, setWarning] = useState<string | null>(null);
  const [confirm, setConfirm] = useState<string | null>(null);
  const [question, setQuestion] = useState("");
  const [asking, setAsking] = useState(false);
  const [answer, setAnswer] = useState<AskResponse | null>(null);

  function pick(m: OfferedModel) {
    if (m.name === current) return;
    if (!m.local && b.modelKind === "local") {
      setConfirm(m.name);
      return;
    }
    void apply(m.name);
  }

  // The response is kept rather than discarded, because the crossing
  // warning only exists in it. useAction would have been the house
  // pattern, but it reports a fixed message and this control's whole
  // job is to say what THIS switch did.
  async function apply(name: string) {
    setBusy(true);
    setError(null);
    try {
      const res = await api.assignProviderModel(b.name, name, b.provider ?? "", endpoint);
      setWarning(res.warning ?? null);
      setConfirm(null);
      say(
        `${b.name} now answers on ${name}${res.crossesToCloud ? " — outside the house" : ""}`,
        res.crossesToCloud ? "warn" : "ok",
      );
      onChanged();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  // Re-check asks the model to operate a tool and restates the verdict.
  // It is the answer to "why is this Butler suddenly questions-only":
  // usually nobody has asked the model yet, and this is the asking.
  async function recheck() {
    setProbing(true);
    setError(null);
    try {
      const res = await api.probeModel(b.model);
      const m = res.model;
      say(
        m.readiness === "ready"
          ? `${modelLabel(b)} passed its tool check`
          : `${modelLabel(b)}: ${m.readinessDetail || m.readiness}`,
        m.readiness === "ready" ? "ok" : "warn",
      );
      onChanged();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setProbing(false);
    }
  }

  async function ask() {
    if (!question.trim()) return;
    if (!row.reachable) {
      say(`${b.name} cannot reach ${modelLabel(b)} right now`, "bad");
      return;
    }
    setAsking(true);
    setError(null);
    try {
      setAnswer(await api.askOneButler(b.name, question.trim()));
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setAsking(false);
    }
  }

  // The crossing dialog names the provider it is about to send to, so
  // it is answered from the offered list the person just picked from
  // rather than from what happens to be configured.
  const to = (offered.data?.models ?? []).find((m) => m.name === confirm);

  return (
    <div className="tab-stack">
      <div className="card">
        <div className="eyebrow">ON NOW</div>
        <div className="row on-now">
          <strong className="on-now-route">{modelLabel(b)}</strong>
          <span className="row">
            <Dot kind={health.kind} />
            <span className={health.kind === "ok" ? "" : `ink-${health.kind}`}>{health.long}</span>
          </span>
        </div>
        <div className="row on-now-foot">
          <span className="muted tnum">
            {[b.provider, b.endpoint].filter(Boolean).join(" · ")}
          </span>
          <button
            className="btn btn-secondary btn-small"
            disabled={probing}
            onClick={() => void recheck()}
          >
            {probing ? "checking…" : "Re-check"}
          </button>
        </div>
      </div>

      {endpoints.length > 1 && (
        <div>
          <div className="eyebrow">FROM</div>
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
        </div>
      )}

      <ErrorLine error={offered.error} />
      {offered.loading && <div className="muted">Asking {endpoint} what it serves…</div>}

      <ModelList
        title="ON YOUR HARDWARE"
        models={(offered.data?.models ?? []).filter((m) => m.local)}
        provider={providerOf}
        current={current}
        busy={busy}
        onPick={pick}
      />
      <ModelList
        title="OUTSIDE THE HOUSE"
        models={(offered.data?.models ?? []).filter((m) => !m.local)}
        provider={providerOf}
        current={current}
        busy={busy}
        onPick={pick}
      />

      {warning && (
        <div className="attention-banner">
          <Dot kind="warn" />
          <div>{warning}</div>
        </div>
      )}
      <ErrorLine error={error} />

      <div className="try-it">
        <div className="eyebrow">TRY IT — asks this Butler on the model above</div>
        <div className="row">
          <input
            aria-label={`Ask ${b.name}`}
            placeholder="how did I sleep last week?"
            value={question}
            onChange={(e) => setQuestion(e.target.value)}
          />
          <button className="btn btn-secondary" disabled={asking || !question.trim()} onClick={ask}>
            {asking ? "asking…" : "Ask"}
          </button>
        </div>
        {answer && (
          <div className="card answer-card">
            <div>{answer.finalText}</div>
            <div className="muted tnum">
              {modelLabel(b)} · {row.leavesTheHouse ? "left the house" : "stayed in the house"} · session{" "}
              {answer.sessionId}
              {answer.mode === "qa-only" && " · questions only, so it looked nothing up"}
            </div>
          </div>
        )}
      </div>

      {confirm && to && (
        <Confirm
          kind="warn"
          icon={<Warning size={15} />}
          title={`This sends ${b.name}'s material outside the house`}
          body={`Everything anyone tells ${b.name} from now on goes to ${confirm}, which runs outside the house. What it already remembers stays where it is.`}
          listTitle="WHAT CHANGES"
          lines={[
            "the conversation open right now carries over on its next message",
            `${row.people} ${row.people === 1 ? "person talks" : "people talk"} to this Butler`,
            "Activity will record the crossing, with your name on it",
          ]}
          confirmLabel="Yes, switch it"
          cancelLabel={`Keep ${modelLabel(b)}`}
          busy={busy}
          onCancel={() => setConfirm(null)}
          onConfirm={() => void apply(confirm)}
        />
      )}
    </div>
  );
}

// One labelled list of models the provider serves.
//
// A model already in use shows as selected even if the provider has
// since stopped listing it — the Butler is still ON it, and a list that
// quietly showed nothing selected would be the one lie this screen must
// not tell. It is drawn as unavailable rather than hidden.
export function ModelList({
  title,
  models,
  provider,
  current,
  busy,
  onPick,
}: {
  title: string;
  models: OfferedModel[];
  // Which provider serves these. Shown on every row rather than once
  // above the list, because two providers can offer a model of the same
  // name and the row is where somebody decides — a heading they scrolled
  // past is not where the ambiguity gets resolved.
  provider: string;
  current: string;
  busy: boolean;
  onPick: (m: OfferedModel) => void;
}) {
  if (models.length === 0) return null;
  return (
    <div>
      <div className="eyebrow">{title}</div>
      <div className="route-list">
        {models.map((m) => {
          const on = m.name === current;
          return (
            <button
              key={m.name}
              type="button"
              role="radio"
              aria-checked={on}
              disabled={busy}
              className={`route-row${on ? " on" : ""}`}
              onClick={() => onPick(m)}
            >
              <span className="radio" aria-hidden />
              {/* Name and provider are one unit so they truncate
                  together and the row stays a single line at any width;
                  the note is flex-none so it is never what gets cut. */}
              <span className="ellip route-name">
                {m.name}
                {provider && <span className="muted"> · {provider}</span>}
              </span>
              <span className={`route-note tnum ${m.local ? "ink-ok" : "ink-warn"}`}>
                {m.local
                  ? m.parameters
                    ? `${m.parameters} · on your hardware`
                    : "on your hardware"
                  : "leaves the house"}
              </span>
            </button>
          );
        })}
      </div>
    </div>
  );
}

// One labelled list of routes.
//
// A route this Butler is on but which somebody has since turned off is
// still listed and still shown as selected. Hiding it would make the
// tab show a model this Butler is not on — the one lie this screen must
// not tell — and A6 is explicit that a Butler with nowhere to run
// WAITS, visibly, rather than being quietly moved.
export function RouteList({
  title,
  routes,
  current,
  busy,
  onPick,
}: {
  title: string;
  routes: Model[];
  current: string;
  busy: boolean;
  onPick: (r: Model) => void;
}) {
  if (routes.length === 0) return null;
  return (
    <div>
      <div className="eyebrow">{title}</div>
      <div className="route-list">
        {routes.map((r) => {
          const on = r.name === current;
          const note = !r.enabled
            ? "turned off in Model routes"
            : r.readiness === "qa-only"
              ? "questions only"
              : r.readiness === "unavailable"
                ? "not answering"
                : r.kind === "cloud"
                  ? "leaves the house"
                  : "ready";
          const kind: Kind = !r.enabled
            ? "mute"
            : r.readiness === "unavailable"
              ? "bad"
              : r.readiness === "qa-only" || r.kind === "cloud"
                ? "warn"
                : "ok";
          return (
            <button
              key={r.name}
              type="button"
              role="radio"
              aria-checked={on}
              disabled={busy}
              className={`route-row${on ? " on" : ""}${r.enabled ? "" : " off"}`}
              onClick={() => onPick(r)}
            >
              <span className="radio" aria-hidden />
              <span>
                {r.name} · {r.provider}
              </span>
              <span className={`route-note tnum ink-${kind}`}>{note}</span>
            </button>
          );
        })}
      </div>
    </div>
  );
}
