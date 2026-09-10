import { useState } from "react";
import { Warning } from "@phosphor-icons/react";
import { api, type AskResponse, type ButlerRow, type Model } from "../../api";
import { ErrorLine } from "../../lib";
import { Confirm } from "../../ui/Confirm";
import { Dot, Kind } from "../../ui/status";
import { Say } from "./ButlerDrawer";

// Which model answers, and what that means for the material.
//
// This tab replaces a `<select>`, and the reason it is bigger than one
// is that a select makes every option look alike. The routes are in two
// labelled lists — ON YOUR HARDWARE and OUTSIDE THE HOUSE — because
// that boundary is the most consequential thing about the choice, and a
// dropdown renders it as a suffix somebody scrolls past.
//
// Switching applies at once (A8). The only friction is local-to-cloud,
// which gets a confirmation, and a disabled route, which gets a toast
// saying where to turn it back on. Cloud-to-local needs neither: it is
// strictly a narrowing.
export function TabModel({
  row,
  routes,
  health,
  say,
  onChanged,
}: {
  row: ButlerRow;
  routes: Model[];
  health: { kind: Kind; long: string };
  say: Say;
  onChanged: () => void;
}) {
  const b = row.butler;
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [warning, setWarning] = useState<string | null>(null);
  const [confirm, setConfirm] = useState<string | null>(null);
  const [question, setQuestion] = useState("");
  const [asking, setAsking] = useState(false);
  const [answer, setAnswer] = useState<AskResponse | null>(null);

  function pick(r: Model) {
    if (r.name === b.model) return;
    if (!r.enabled) {
      say(`${r.name} is turned off in Model routes`, "warn");
      return;
    }
    if (r.kind === "cloud" && b.modelKind === "local") {
      setConfirm(r.name);
      return;
    }
    void apply(r.name);
  }

  // The response is kept rather than discarded, because the crossing
  // warning only exists in it. useAction would have been the house
  // pattern, but it reports a fixed message and this control's whole
  // job is to say what THIS switch did.
  async function apply(name: string) {
    setBusy(true);
    setError(null);
    try {
      const res = await api.setButlerModel(b.name, name);
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

  async function ask() {
    if (!question.trim()) return;
    if (!row.reachable) {
      say(`${b.name} cannot reach ${b.model} right now`, "bad");
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

  const to = routes.find((r) => r.name === confirm);

  return (
    <div className="tab-stack">
      <div className="card">
        <div className="eyebrow">ON NOW</div>
        <div className="row on-now">
          <strong className="on-now-route">{b.model}</strong>
          <span className="row">
            <Dot kind={health.kind} />
            <span className={health.kind === "ok" ? "" : `ink-${health.kind}`}>{health.long}</span>
          </span>
        </div>
        <div className="muted tnum">
          {[b.provider, b.endpoint].filter(Boolean).join(" · ")}
        </div>
      </div>

      <RouteList
        title="ON YOUR HARDWARE"
        routes={routes.filter((r) => r.kind === "local")}
        current={b.model}
        busy={busy}
        onPick={pick}
      />
      <RouteList
        title="OUTSIDE THE HOUSE"
        routes={routes.filter((r) => r.kind === "cloud")}
        current={b.model}
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
              {b.model} · {row.leavesTheHouse ? "left the house" : "stayed in the house"} · session{" "}
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
          body={`Everything anyone tells ${b.name} from now on goes to ${to.provider}. What it already remembers stays where it is.`}
          listTitle="WHAT CHANGES"
          lines={[
            "the conversation open right now carries over on its next message",
            `${row.people} ${row.people === 1 ? "person talks" : "people talk"} to this Butler`,
            "Activity will record the crossing, with your name on it",
          ]}
          confirmLabel="Yes, switch it"
          cancelLabel={`Keep ${b.model}`}
          busy={busy}
          onCancel={() => setConfirm(null)}
          onConfirm={() => void apply(confirm)}
        />
      )}
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
