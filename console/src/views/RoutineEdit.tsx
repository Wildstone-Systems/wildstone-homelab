import { useState } from "react";
import { ArrowLeft } from "@phosphor-icons/react";
import { api, type AutomationGrammar, type RoutineDetail as Detail } from "../api";
import type { AutomationSpec } from "../generated/resources";
import { ErrorLine, useAction, useLoad } from "../lib";
import type { Mode } from "./Routines";

// Changing what a routine does (ADR-W024 A5, ADR-W030 A6).
//
// Every control here is a choice from a closed vocabulary the server
// serves, never free text. A routine that triggers on an event nothing
// emits never fires and nothing says why; an action above the domestic
// tier is refused by the automation-action-tier rule. Offering either
// would be offering a rejection.
//
// The form edits the spec the detail read served and sends that back.
// The browser has no YAML writer and does not grow one: the server
// parses this grammar and the server writes it, so there is one
// implementation of it rather than two that can disagree.

// WINDOWS is the condition window vocabulary the plan settles on. The
// schema allows more, but these are the four a household reasons about.
const WINDOWS = ["1h", "2h", "3h", "4h"];

// What the form says before the save, rather than after.
//
// Editing a routine that is ACTING takes it out of action: there is no
// approval step on this path, so returning it to trial is the only thing
// keeping ADR-W024's promise that a routine is watched before it acts.
// Someone about to press Save should know that, and should not be told
// anything at all until they have actually changed something.
export function changeWarning(changed: boolean, acting: boolean, revision: number): string | null {
  if (!changed) return null;
  const base = `Changing this changes what the house does. It becomes rev ${revision + 1}.`;
  if (!acting) return base;
  return (
    base +
    " This routine is acting now, so saving puts it back in trial: it will watch and report until you let it act again."
  );
}

export function RoutineEdit({
  name,
  onMode,
  onSaved,
}: {
  name: string;
  onMode: (m: Mode) => void;
  onSaved: () => void;
}) {
  const detail = useLoad(() => api.householdRoutine(name), [name]);
  const grammar = useLoad(() => api.automationGrammar());
  return (
    <>
      <div className="row">
        <button className="btn btn-ghost" onClick={() => onMode({ kind: "detail", name })}>
          <ArrowLeft size={15} /> {name}
        </button>
      </div>
      <ErrorLine error={detail.error} />
      <ErrorLine error={grammar.error} />
      {!detail.data && detail.loading && <div className="muted">Loading…</div>}
      {detail.data && (
        <EditForm
          name={name}
          detail={detail.data}
          grammar={grammar.data}
          onMode={onMode}
          onSaved={onSaved}
        />
      )}
    </>
  );
}

export function EditForm({
  name,
  detail,
  grammar,
  onMode,
  onSaved,
}: {
  name: string;
  detail: Detail;
  grammar: AutomationGrammar | null;
  onMode: (m: Mode) => void;
  onSaved: () => void;
}) {
  const original = detail.spec;
  // The whole spec is held in state, not just the fields rendered below.
  // A form that rebuilt the spec from its own inputs would silently drop
  // the context and escalation blocks it never showed — a data-loss bug
  // wearing the shape of a save.
  const [spec, setSpec] = useState<AutomationSpec | null>(original);
  const [note, setNote] = useState("");
  const action = useAction();

  if (!spec || !original) {
    return (
      <div className="callout callout-warn">
        This routine's document does not parse, so it cannot be edited as a form. Fix the document
        first — the detail screen shows it as written.
      </div>
    );
  }

  const condition = spec.conditions?.[0];
  const changed = JSON.stringify(spec) !== JSON.stringify(original);
  const acting = detail.routine.state === "enabled";
  const warning = changeWarning(changed, acting, detail.routine.revision);

  // Every edit replaces one field of a nested structure. Doing it
  // immutably keeps `changed` honest.
  const setCondition = (patch: Partial<NonNullable<AutomationSpec["conditions"]>[number]>) =>
    setSpec({
      ...spec,
      conditions: (spec.conditions ?? []).map((c, i) => (i === 0 ? { ...c, ...patch } : c)),
    });

  const save = () =>
    action.run(async () => {
      await api.editRoutine(name, spec, note, detail.routine.revision);
      onSaved();
      onMode({ kind: "detail", name });
    }, "Saved.");

  return (
    <>
      <div className="sub">
        One routine's spec. Every choice here is one the engine will accept — the lists come from
        what your packages actually publish and the tiers a household rule may carry.
      </div>

      <div className="panel" style={{ maxWidth: "52ch" }}>
        <div className="field">
          <label>Trigger</label>
          <select
            value={spec.trigger.eventType}
            onChange={(e) => setSpec({ ...spec, trigger: { ...spec.trigger, eventType: e.target.value } })}
          >
            {/* The routine's current trigger stays selectable even if the
                package that published it has since been removed: a form
                that silently re-points a rule at a different event is
                worse than one that shows an event nothing emits. */}
            {!grammar?.eventTypes.some((e) => e.type === spec.trigger.eventType) && (
              <option value={spec.trigger.eventType}>
                {spec.trigger.eventType} — nothing installed publishes this
              </option>
            )}
            {(grammar?.eventTypes ?? []).map((e) => (
              <option key={e.type} value={e.type}>
                {e.type}
                {e.description ? ` — ${e.description}` : ""}
              </option>
            ))}
          </select>
        </div>

        {condition && (
          <>
            <div className="field">
              <label>Condition window</label>
              <select
                value={condition.window ?? ""}
                onChange={(e) => setCondition({ window: e.target.value || undefined })}
              >
                <option value="">no window</option>
                {WINDOWS.map((w) => (
                  <option key={w} value={w}>
                    {w}
                  </option>
                ))}
              </select>
            </div>
            <div className="field">
              <label>After</label>
              <input
                type="time"
                value={condition.after ?? ""}
                onChange={(e) => setCondition({ after: e.target.value || undefined })}
              />
            </div>
            <div className="field">
              <label>Before</label>
              <input
                type="time"
                value={condition.before ?? ""}
                onChange={(e) => setCondition({ before: e.target.value || undefined })}
              />
            </div>
          </>
        )}

        <div className="field">
          <label>Action</label>
          <select
            value={spec.action.tool}
            onChange={(e) => {
              // The tier belongs to the tool, not to a separate choice:
              // a tool's declared risk IS the tier a rule carries, and
              // letting someone pick them apart invents combinations the
              // policy engine refuses.
              const picked = grammar?.actions.find((a) => a.tool === e.target.value);
              setSpec({
                ...spec,
                action: { ...spec.action, tool: e.target.value, tier: picked?.tier ?? spec.action.tier },
              });
            }}
          >
            {!grammar?.actions.some((a) => a.tool === spec.action.tool) && (
              <option value={spec.action.tool}>
                {spec.action.tool} — nothing installed offers this
              </option>
            )}
            {(grammar?.actions ?? []).map((a) => (
              <option key={a.tool} value={a.tool}>
                {a.tool} · tier {a.tier}
              </option>
            ))}
          </select>
        </div>

        <div className="field">
          <label>What changed</label>
          <input
            type="text"
            placeholder="Why you changed it"
            value={note}
            onChange={(e) => setNote(e.target.value)}
          />
        </div>
      </div>

      {warning && (
        <div className="callout callout-warn" style={{ maxWidth: "52ch" }}>
          {warning}
        </div>
      )}

      {action.state.phase === "error" && <ErrorLine error={action.state.message} />}

      <div className="row">
        <button
          className="btn btn-secondary"
          onClick={() => onMode({ kind: "dryrun", name, spec, rule: detail.rule })}
        >
          Dry-run the change
        </button>
        <button
          className="btn btn-primary"
          style={{ marginLeft: "auto" }}
          disabled={!changed || action.state.phase === "busy"}
          onClick={save}
        >
          Save
        </button>
      </div>
    </>
  );
}
