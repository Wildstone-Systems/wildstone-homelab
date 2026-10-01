import { useEffect, useMemo, useState } from "react";
import { ArrowRight, Info, SquaresFour } from "@phosphor-icons/react";
import {
  api,
  ApiError,
  BluetoothMode,
  HomeAutomationAction,
  HomeAutomationBlocker,
  HomeAutomationComponent,
  HomeAutomationComponentId,
  HomeAutomationContinuityView,
  HomeAutomationEvidence,
  HomeAutomationNode,
  HomeAutomationQuery,
  HomeAutomationRadio,
  HomeAutomationRadioOwner,
  HomeAutomationStatus,
  HomeAutomationTopology,
  Radio,
  RadioKind,
  ReadinessCheckName,
  ReadinessState,
} from "../api";
import { ago, ErrorLine, useLoad } from "../lib";
import { Kind, StatusPill } from "../ui/status";
import { SetupState, SetupStep } from "./HomeAutomationSetup";
import { CareClient, Panel, present, StatusPage } from "./HomeAutomationStatus";

// Home Automation (#448, epic #431): the household's way in to Home
// Assistant and its radios, instead of four package forms and a device
// path typed from memory.
//
// Everything this screen says about what EXISTS comes from GET
// /api/v1/home-automation (#446), which is the one authority for what is
// installed, pending, blocked, and what depends on what. What the person
// has only CHOSEN here — a topology, a machine, the protocols they want
// — is a draft: it is kept in this browser so leaving and coming back
// does not lose it, it is sent to the aggregate as goals (which the API
// never stores), and it is never shown as anything but a choice. Nothing
// is installed until a plan is approved, and this screen approves none.
//
// The flow is steps so later slices slot in without reshaping it:
// "choose" is this slice (#448); "readiness" is where the prerequisite
// evidence and radio picker land (#449); "setup" drives the ordered plan
// sequence (#450, HomeAutomationSetup.tsx). Once anything is installed
// the screen opens on the composed status page instead (#451,
// HomeAutomationStatus.tsx): "choose" then means "status", and the
// choices are reached as "add", for adding a kind of device later.

export type Step = "choose" | "add" | "readiness" | "setup" | "status";

// A goal is what a household asks for, which is not always one
// component: Matter over Thread needs the Matter server AND a Thread
// border router. The mapping to component ids is only which components
// to ASK the API about — whether they are ready, blocked or need more
// is still the API's answer.
export type Goal = "zigbee" | "zwave" | "matter-wifi" | "matter-thread";
export const GOALS: Goal[] = ["zigbee", "zwave", "matter-wifi", "matter-thread"];

const GOAL_COMPONENTS: Record<Goal, HomeAutomationComponentId[]> = {
  zigbee: ["zigbee"],
  zwave: ["zwave"],
  "matter-wifi": ["matter"],
  "matter-thread": ["matter", "thread"],
};

// What each choice means for the hardware and the network, in the words
// a household would use when deciding. These are consequences to weigh,
// not a verdict about THIS machine: whether a radio is present, suitable
// or already owned comes from the API on the readiness step.
export function goalComponents(g: Goal): HomeAutomationComponentId[] {
  return GOAL_COMPONENTS[g];
}

export const GOAL_COPY: Record<Goal, { title: string; body: string }> = {
  zigbee: {
    title: "Zigbee",
    body:
      "Needs a Zigbee USB coordinator plugged into the chosen machine. Adds Zigbee2MQTT and an MQTT broker; " +
      "devices show up in Home Assistant through MQTT.",
  },
  zwave: {
    title: "Z-Wave",
    body:
      "Needs a Z-Wave USB stick plugged into the chosen machine. Adds Z-Wave JS UI, which Home Assistant " +
      "connects to over the network.",
  },
  "matter-wifi": {
    title: "Matter over Wi-Fi",
    body:
      "No extra radio: devices join your Wi-Fi. The machine must have IPv6 and multicast working on its " +
      "network interface. Adds the Matter server.",
  },
  "matter-thread": {
    title: "Matter over Thread",
    body:
      "Needs a Thread radio (a USB dongle with RCP firmware) on the chosen machine, IPv6 forwarding and a " +
      "TUN device. Adds a Thread border router alongside the Matter server; your phone joins the Thread " +
      "network through the Home Assistant app.",
  },
};

export type BluetoothChoice = "none" | BluetoothMode;

export const BLUETOOTH_COPY: Record<BluetoothChoice, { title: string; body: string }> = {
  none: { title: "Not needed", body: "Home Assistant runs without Bluetooth." },
  "host-bluez": {
    title: "An adapter in this machine",
    body:
      "Uses a Bluetooth adapter in the chosen machine through BlueZ, so the host needs D-Bus and the " +
      "Bluetooth service running. Reaches a room or two around the machine.",
  },
  proxy: {
    title: "Bluetooth proxies",
    body:
      "Uses small ESP32 boards running ESPHome placed around the house, on your network. Needs no adapter " +
      "here and reaches wherever a proxy is.",
  },
};

// The draft: only choices, never facts. Every field is optional because
// "not chosen yet" is a real answer the screen has to render.
//
// `radios` holds the radio picked per component on the readiness step,
// by the inventory's stable radio `id` (never a /dev path): it is what
// the setup sequence proposes with. `manualRadios` is the advanced
// escape hatch — a device path typed by hand for a radio the inventory
// does not list — and is only ever set from that explicit affordance.
//
// `options` are package params only the household knows, typed on the
// setup step when the orchestration asks for one (provide_option).
// `confirmedSteps` are "<component>/<step id>" steps in Home Assistant
// the person says they have done: Wildstone cannot see them, so they
// are the person's word, kept here and sent on every call — never a
// verification, and always undoable.
export interface Draft {
  topology?: HomeAutomationTopology;
  node?: string;
  goals: Goal[];
  bluetooth: BluetoothChoice;
  step: Step;
  radios?: Partial<Record<HomeAutomationComponentId, string>>;
  manualRadios?: Partial<Record<HomeAutomationComponentId, string>>;
  options?: Partial<Record<HomeAutomationComponentId, Record<string, string>>>;
  confirmedSteps?: string[];
}

const COMPONENT_IDS: HomeAutomationComponentId[] = [
  "mqtt", "home-assistant", "home-automation", "zigbee", "zwave", "matter", "thread", "bluetooth",
];

// A per-component string map from storage, keeping only known component
// ids with non-empty string values; undefined when nothing survives.
function componentMap(v: unknown): Partial<Record<HomeAutomationComponentId, string>> | undefined {
  if (!v || typeof v !== "object" || Array.isArray(v)) return undefined;
  const out: Partial<Record<HomeAutomationComponentId, string>> = {};
  for (const id of COMPONENT_IDS) {
    const x = (v as Record<string, unknown>)[id];
    if (typeof x === "string" && x) out[id] = x;
  }
  return Object.keys(out).length ? out : undefined;
}

// Per-component param values from storage: known component ids, each a
// map of param-shaped names to non-empty strings.
function optionMap(v: unknown): Draft["options"] {
  if (!v || typeof v !== "object" || Array.isArray(v)) return undefined;
  const out: NonNullable<Draft["options"]> = {};
  for (const id of COMPONENT_IDS) {
    const x = (v as Record<string, unknown>)[id];
    if (!x || typeof x !== "object" || Array.isArray(x)) continue;
    const params: Record<string, string> = {};
    for (const [k, val] of Object.entries(x as Record<string, unknown>)) {
      if (/^[A-Za-z0-9_]+$/.test(k) && typeof val === "string" && val) params[k] = val;
    }
    if (Object.keys(params).length) out[id] = params;
  }
  return Object.keys(out).length ? out : undefined;
}

// Confirmations from storage: "<known component>/<step id>" only.
function confirmedList(v: unknown): string[] | undefined {
  if (!Array.isArray(v)) return undefined;
  const out = v.filter(
    (k): k is string =>
      typeof k === "string" &&
      /^[a-z-]+\/[A-Za-z0-9_.-]+$/.test(k) &&
      COMPONENT_IDS.includes(k.split("/")[0] as HomeAutomationComponentId),
  );
  return out.length ? [...new Set(out)] : undefined;
}

export const EMPTY_DRAFT: Draft = { goals: [], bluetooth: "none", step: "choose" };

const DRAFT_KEY = "wildstone.homeAutomation.draft";

// Reads back only what it recognises. A draft written by an older Console,
// hand-edited, or from a storage that throws all read as a clean start
// rather than as a crash or a half-applied choice.
export function loadDraft(storage: Pick<Storage, "getItem"> | undefined = safeLocalStorage()): Draft {
  try {
    const raw: unknown = JSON.parse(storage?.getItem(DRAFT_KEY) ?? "null");
    if (!raw || typeof raw !== "object") return EMPTY_DRAFT;
    const r = raw as Record<string, unknown>;
    const topology = r.topology === "managed" || r.topology === "external" ? r.topology : undefined;
    const goals = Array.isArray(r.goals)
      ? GOALS.filter((g) => (r.goals as unknown[]).includes(g))
      : [];
    const bluetooth: BluetoothChoice =
      r.bluetooth === "host-bluez" || r.bluetooth === "proxy" ? r.bluetooth : "none";
    const step: Step =
      r.step === "readiness" && topology === "managed"
        ? "readiness"
        : r.step === "setup" && topology
          ? "setup"
          : r.step === "add" && topology
            ? "add"
            : r.step === "status"
              ? "status"
              : "choose";
    const node = typeof r.node === "string" && r.node ? r.node : undefined;
    const d: Draft = { topology, node, goals, bluetooth, step };
    const radios = componentMap(r.radios);
    const manualRadios = componentMap(r.manualRadios);
    if (radios) d.radios = radios;
    if (manualRadios) d.manualRadios = manualRadios;
    const options = optionMap(r.options);
    const confirmedSteps = confirmedList(r.confirmedSteps);
    if (options) d.options = options;
    if (confirmedSteps) d.confirmedSteps = confirmedSteps;
    return d;
  } catch {
    return EMPTY_DRAFT;
  }
}

export function saveDraft(d: Draft, storage: Pick<Storage, "setItem"> | undefined = safeLocalStorage()) {
  try {
    storage?.setItem(DRAFT_KEY, JSON.stringify(d));
  } catch {
    // Private mode or storage disabled: the choices hold for this visit
    // and are simply not there next time.
  }
}

function safeLocalStorage(): Storage | undefined {
  try {
    return typeof localStorage === "undefined" ? undefined : localStorage;
  } catch {
    return undefined;
  }
}

// The goals the API is asked about. Bluetooth is Home Assistant's own
// setting, so it is only a goal on the managed path.
export function queryOf(d: Draft): HomeAutomationQuery {
  const select = new Set<HomeAutomationComponentId>();
  for (const g of d.goals) for (const c of GOAL_COMPONENTS[g]) select.add(c);
  const bluetooth = d.topology === "managed" && d.bluetooth !== "none" ? d.bluetooth : undefined;
  if (bluetooth) select.add("bluetooth");
  return {
    topology: d.topology,
    node: d.node,
    select: [...select],
    bluetooth,
  };
}

// The topology to preselect when nothing has been chosen here yet: what
// the API says is already installed or pending, so a household that
// already runs one is not offered a blank page.
function effectiveTopology(d: Draft, status: HomeAutomationStatus | null): HomeAutomationTopology | undefined {
  if (d.topology) return d.topology;
  switch (status?.topology) {
    case "managed":
    case "both":
      return "managed";
    case "external":
      return "external";
  }
  return undefined;
}

export function HomeAutomation({
  onOpenPackage,
  onOpenServices,
  onNavigate,
}: {
  onOpenPackage: (pkg: string) => void;
  onOpenServices: () => void;
  onNavigate?: Navigate;
}) {
  const [draft, setDraftState] = useState<Draft>(() => loadDraft());
  const setDraft = (d: Draft) => {
    setDraftState(d);
    saveDraft(d);
  };
  const q = queryOf(draft);
  const key = JSON.stringify(q);
  const status = useLoad(async () => {
    try {
      return await api.homeAutomation(q);
    } catch (e) {
      // A machine chosen last time may have been removed since. Ask
      // again without it rather than leaving the screen on an error the
      // person cannot act on; the node picker then shows what exists.
      if (q.node && e instanceof ApiError && e.status === 400 && /unknown node/.test(e.message)) {
        return api.homeAutomation({ ...q, node: undefined });
      }
      throw e;
    }
  }, [key]);

  // What each installed part keeps where, for the status page's radio
  // and removal explanations. Asked again whenever the aggregate is.
  const established = !!status.data?.components.some(present);
  const continuity = useLoad(
    async () => (established ? api.homeAutomationContinuity() : null),
    [established, status.data?.generatedAt],
  );

  // Drop a remembered machine the API no longer knows, so the draft
  // stops asking about it.
  const nodes = status.data?.nodes;
  useEffect(() => {
    if (draft.node && nodes && !nodes.some((n) => n.id === draft.node)) {
      setDraft({ ...draft, node: undefined });
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nodes]);

  if (!status.data) {
    return status.error ? (
      <ErrorLine error={status.error} />
    ) : (
      <div className="muted">Checking what is set up for home automation…</div>
    );
  }
  return (
    <HomeAutomationView
      status={status.data}
      draft={draft}
      onDraft={setDraft}
      loading={status.loading}
      error={status.error}
      onRefresh={status.reload}
      onOpenPackage={onOpenPackage}
      onOpenServices={onOpenServices}
      onNavigate={onNavigate}
      continuity={continuity.data}
      continuityError={continuity.error}
    />
  );
}

// Separated from its fetch so the render check can put it in each state.
export function HomeAutomationView({
  status,
  draft,
  onDraft,
  loading = false,
  error = null,
  onRefresh = () => {},
  onOpenPackage,
  onOpenServices,
  onNavigate,
  setup,
  continuity = null,
  continuityError = null,
  care,
  panel,
}: {
  status: HomeAutomationStatus;
  draft: Draft;
  onDraft: (d: Draft) => void;
  loading?: boolean;
  error?: string | null;
  onRefresh?: () => void;
  onOpenPackage: (pkg: string) => void;
  onOpenServices: () => void;
  onNavigate?: Navigate;
  // The setup step's state, for the render check; the screen fetches it.
  setup?: SetupState;
  continuity?: HomeAutomationContinuityView | null;
  continuityError?: string | null;
  // The status page's repair calls and an open panel, for the render check.
  care?: CareClient;
  panel?: Panel;
}) {
  const topology = effectiveTopology(draft, status);
  const comps = useMemo(() => new Map(status.components.map((c) => [c.id, c])), [status.components]);
  // Once anything is installed or on its way, the screen is its status;
  // the choices are for adding to it.
  const established = status.components.some(present);
  const step: Step =
    draft.step === "choose" && established ? "status" : draft.step === "status" && !established ? "choose" : draft.step;
  const chooseStep: Step = established ? "add" : "choose";
  const actionHandlers: ActionHandlers = {
    onBack: () => onDraft({ ...draft, step: chooseStep }),
    onRefresh,
    loading,
    onOpenPackage,
    onOpenServices,
    onNavigate,
  };

  return (
    <div className="ha">
      <div className="sub">
        Run Home Assistant on one of your machines, or connect the one you already have, and choose the
        kinds of devices you want it to reach. Choices here are only choices: nothing is installed until you
        approve a plan.
      </div>
      <ErrorLine error={error} />
      {established && step !== "status" && (
        <div className="row ha-back">
          <button type="button" className="btn btn-ghost btn-sm" onClick={() => onDraft({ ...draft, step: "status" })}>
            Back to status
          </button>
        </div>
      )}
      {step !== "status" && <InstalledNow status={status} />}
      {status.blockers.map((b) => (
        <div key={b.code} className={b.severity === "blocking" ? "callout callout-bad" : "callout callout-warn"} role="note">
          {b.message}
        </div>
      ))}

      {step === "status" ? (
        <StatusPage
          status={status}
          continuity={continuity}
          continuityError={continuityError}
          draft={draft}
          onDraft={onDraft}
          handlers={actionHandlers}
          client={care}
          initialPanel={panel}
        />
      ) : step === "setup" && topology ? (
        <SetupStep
          status={status}
          topology={topology}
          draft={draft}
          onDraft={onDraft}
          handlers={{
            // Back to where the choices it proposes from are made: the
            // radio picker on the managed path, the choices otherwise.
            onBack: () => onDraft({ ...draft, topology, step: topology === "managed" ? "readiness" : chooseStep }),
            onChooseRadio: topology === "managed" ? () => onDraft({ ...draft, topology, step: "readiness" }) : undefined,
            onRefresh,
            loading,
            onOpenPackage,
            onOpenServices,
            onNavigate,
          }}
          setup={setup}
        />
      ) : step === "readiness" && topology === "managed" ? (
        <ReadinessStep
          status={status}
          comps={comps}
          draft={draft}
          onDraft={onDraft}
          handlers={{
            onBack: () => onDraft({ ...draft, step: chooseStep }),
            onContinue: () => onDraft({ ...draft, step: "setup" }),
            onRefresh,
            loading,
            onOpenPackage,
            onOpenServices,
            onNavigate,
          }}
        />
      ) : (
        <>
          {step === "add" && topology ? (
            // Adding to what runs: how Home Assistant runs and on which
            // machine are already decided by what is installed.
            <div className="card ha-card">
              <h3>Add to your home automation</h3>
              <div className="muted">
                {topology === "managed" ? "Home Assistant runs here" : "Connected to your own Home Assistant"}
                {status.node ? `, on ${status.node}` : ""}. Choose what to add; what is installed stays as it is.
              </div>
            </div>
          ) : (
            <TopologyChoice
              value={topology}
              comps={comps}
              onChange={(t) => onDraft({ ...draft, topology: t, step: chooseStep })}
            />
          )}
          {topology && (
            <>
              {step !== "add" && (
                <NodeChoice
                  status={status}
                  value={draft.node}
                  // Radio picks name one machine's radios, so another
                  // machine starts without them.
                  onChange={(n) => onDraft({ ...draft, node: n, radios: undefined, manualRadios: undefined })}
                />
              )}
              <GoalChoice draft={draft} topology={topology} comps={comps} onDraft={onDraft} />
              <NextStep
                topology={topology}
                draft={draft}
                comps={comps}
                onReadiness={() => onDraft({ ...draft, topology, step: "readiness" })}
                onSetup={() => onDraft({ ...draft, topology, step: "setup" })}
                onOpenPackage={onOpenPackage}
              />
            </>
          )}
        </>
      )}

      <div className="ha-advanced">
        <SquaresFour size={16} aria-hidden />
        <span>
          Prefer the package forms? Every part of this is also an ordinary service.{" "}
          <button type="button" className="btn btn-ghost btn-sm" onClick={onOpenServices}>
            Open Services
          </button>
        </span>
      </div>
    </div>
  );
}

// What the API says is installed or waiting on a plan, and nothing else.
// A draft choice never appears here.
function InstalledNow({ status }: { status: HomeAutomationStatus }) {
  const present = status.components.filter((c) => c.selection === "installed" || c.selection === "pending");
  return (
    <div className="card ha-now" aria-live="polite">
      <div className="ha-now-head">
        <h3>Set up now</h3>
        <span className="muted">checked {ago(status.generatedAt)}</span>
      </div>
      {present.length === 0 ? (
        <div className="muted">Nothing for home automation is installed yet.</div>
      ) : (
        <ul className="ha-list">
          {present.map((c) => (
            <li key={c.id}>
              <span>{c.title}</span>
              <ComponentPill c={c} />
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// The component's standing, in words that never promote a choice to an
// installation: only `selection: installed` may read as installed.
export function componentStanding(c: HomeAutomationComponent): { kind: Kind; label: string } {
  if (c.state === "unavailable" && c.selection !== "installed") return { kind: "mute", label: "Not available here" };
  if (c.selection === "installed") {
    switch (c.state) {
      case "ready":
        return { kind: "ok", label: "Installed" };
      case "degraded":
        return { kind: "bad", label: "Installed, needs attention" };
      case "external_step":
        return { kind: "warn", label: "Installed, a step in Home Assistant is left" };
      case "awaiting_approval":
        return { kind: "warn", label: "Installed, a change waits for approval" };
      case "applying":
        return { kind: "warn", label: "Installed, a change is being applied" };
      default:
        return { kind: "warn", label: "Installed, needs attention" };
    }
  }
  if (c.selection === "pending") {
    return c.state === "applying"
      ? { kind: "warn", label: "Being installed" }
      : { kind: "warn", label: "Plan waiting for approval" };
  }
  return { kind: "mute", label: "Not installed" };
}

export function ComponentPill({ c }: { c: HomeAutomationComponent }) {
  const s = componentStanding(c);
  return (
    <StatusPill kind={s.kind} title={c.reason}>
      {s.label}
    </StatusPill>
  );
}

function TopologyChoice({
  value,
  comps,
  onChange,
}: {
  value?: HomeAutomationTopology;
  comps: Map<HomeAutomationComponentId, HomeAutomationComponent>;
  onChange: (t: HomeAutomationTopology) => void;
}) {
  const options: { id: HomeAutomationTopology; comp: HomeAutomationComponentId; title: string; body: string }[] = [
    {
      id: "external",
      comp: "home-automation",
      title: "Connect existing Home Assistant",
      body:
        "You already run Home Assistant somewhere. Wildstone adds a small connector so the household's " +
        "Butlers can see and use it, and can run radios for it on one of your machines.",
    },
    {
      id: "managed",
      comp: "home-assistant",
      title: "Run it here",
      body:
        "Wildstone runs Home Assistant on one of your machines, on your home network, and keeps it " +
        "updated and backed up like any other service.",
    },
  ];
  return (
    <fieldset className="ha-fieldset">
      <legend className="ha-legend">1. How will Home Assistant run?</legend>
      <div className="ha-options ha-options-2">
        {options.map((o) => {
          const c = comps.get(o.comp);
          return (
            <label key={o.id} className={value === o.id ? "ha-option on" : "ha-option"}>
              <input
                type="radio"
                name="ha-topology"
                value={o.id}
                checked={value === o.id}
                onChange={() => onChange(o.id)}
              />
              <span className="ha-option-body">
                <span className="ha-option-title">{o.title}</span>
                <span className="ha-option-text">{o.body}</span>
                {c && (c.selection === "installed" || c.selection === "pending") && <ComponentPill c={c} />}
              </span>
            </label>
          );
        })}
      </div>
    </fieldset>
  );
}

function NodeChoice({
  status,
  value,
  onChange,
}: {
  status: HomeAutomationStatus;
  value?: string;
  onChange: (n: string) => void;
}) {
  // The API resolves the machine when none was chosen (the one already
  // running these, or the only one), so its answer is what is shown as
  // selected — the person sees the machine the rest of the answer is
  // about.
  const current = value ?? status.node;
  if (status.nodes.length === 0) {
    return (
      <fieldset className="ha-fieldset">
        <legend className="ha-legend">2. Which machine?</legend>
        <div className="muted">No machines have joined yet. Pair one under Nodes first.</div>
      </fieldset>
    );
  }
  return (
    <fieldset className="ha-fieldset">
      <legend className="ha-legend">2. Which machine?</legend>
      <div className="muted ha-hint">Radios plug into this machine, and it needs to be on the same network as your devices.</div>
      <div className="ha-options">
        {status.nodes.map((n) => (
          <label key={n.id} className={current === n.id ? "ha-option on" : "ha-option"}>
            <input
              type="radio"
              name="ha-node"
              value={n.id}
              checked={current === n.id}
              onChange={() => onChange(n.id)}
            />
            <span className="ha-option-body">
              <span className="ha-option-title">{n.id}</span>
              <span className="ha-option-text">
                {n.connected ? "Connected" : `Offline, last seen ${ago(n.lastSeenAt)}`}
                {n.hostAddress ? ` · ${n.hostAddress}` : ""}
              </span>
            </span>
          </label>
        ))}
      </div>
    </fieldset>
  );
}

// The standing of a goal's components, as the API reports them. A goal
// is "installed" only when every component it needs is.
function goalInstalled(g: Goal, comps: Map<HomeAutomationComponentId, HomeAutomationComponent>): boolean {
  return GOAL_COMPONENTS[g].every((id) => comps.get(id)?.selection === "installed");
}

function GoalChoice({
  draft,
  topology,
  comps,
  onDraft,
}: {
  draft: Draft;
  topology: HomeAutomationTopology;
  comps: Map<HomeAutomationComponentId, HomeAutomationComponent>;
  onDraft: (d: Draft) => void;
}) {
  const toggle = (g: Goal, on: boolean) => {
    const goals = on ? [...new Set([...draft.goals, g])] : draft.goals.filter((x) => x !== g);
    onDraft({ ...draft, topology, goals: GOALS.filter((x) => goals.includes(x)) });
  };
  const bt = comps.get("bluetooth");
  const btInstalled = bt?.selection === "installed" ? (bt.mode as BluetoothChoice | undefined) : undefined;

  return (
    <fieldset className="ha-fieldset">
      <legend className="ha-legend">3. Which kinds of devices? (optional)</legend>
      <div className="muted ha-hint">
        Choose only what you have or plan to buy. You can add more later.
      </div>
      <div className="ha-options">
        {GOALS.map((g) => {
          const installed = goalInstalled(g, comps);
          const parts = GOAL_COMPONENTS[g].map((id) => comps.get(id)).filter((c): c is HomeAutomationComponent => !!c);
          const unavailable = parts.find((c) => c.state === "unavailable" && c.selection !== "installed");
          const checked = installed || draft.goals.includes(g);
          const id = `ha-goal-${g}`;
          return (
            <label key={g} className={checked ? "ha-option on" : "ha-option"}>
              <input
                id={id}
                type="checkbox"
                checked={checked}
                // Removing something installed is its own decision, made
                // from its status (#451), not by unticking a box here.
                disabled={installed || (!!unavailable && !draft.goals.includes(g))}
                onChange={(e) => toggle(g, e.target.checked)}
                aria-labelledby={`${id}-title`}
                aria-describedby={`${id}-desc`}
              />
              <span className="ha-option-body">
                <span id={`${id}-title`} className="ha-option-title">
                  {GOAL_COPY[g].title}
                </span>
                <span id={`${id}-desc`} className="ha-option-text">
                  {GOAL_COPY[g].body}
                  {unavailable && <span className="ha-option-note"> Not available here: {unavailable.reason}</span>}
                </span>
                <span className="ha-pills">
                  {parts.map((c) => (
                    <span key={c.id} className="ha-pill-line">
                      {parts.length > 1 && <span className="muted">{c.title}</span>}
                      <ComponentPill c={c} />
                    </span>
                  ))}
                  {!installed && draft.goals.includes(g) && (
                    <StatusPill kind="mute">Chosen</StatusPill>
                  )}
                </span>
              </span>
            </label>
          );
        })}
      </div>

      {topology === "managed" && (
        <fieldset className="ha-fieldset ha-nested">
          <legend className="ha-legend-sub">Bluetooth</legend>
          <div className="ha-options">
            {(Object.keys(BLUETOOTH_COPY) as BluetoothChoice[]).map((m) => {
              const chosen = btInstalled ?? draft.bluetooth;
              return (
                <label key={m} className={chosen === m ? "ha-option on" : "ha-option"}>
                  <input
                    type="radio"
                    name="ha-bluetooth"
                    value={m}
                    checked={chosen === m}
                    disabled={!!btInstalled}
                    onChange={() => onDraft({ ...draft, topology, bluetooth: m })}
                  />
                  <span className="ha-option-body">
                    <span className="ha-option-title">{BLUETOOTH_COPY[m].title}</span>
                    <span className="ha-option-text">{BLUETOOTH_COPY[m].body}</span>
                    {btInstalled === m && bt && <ComponentPill c={bt} />}
                  </span>
                </label>
              );
            })}
          </div>
        </fieldset>
      )}
    </fieldset>
  );
}

function NextStep({
  topology,
  draft,
  comps,
  onReadiness,
  onSetup,
  onOpenPackage,
}: {
  topology: HomeAutomationTopology;
  draft: Draft;
  comps: Map<HomeAutomationComponentId, HomeAutomationComponent>;
  onReadiness: () => void;
  onSetup: () => void;
  onOpenPackage: (pkg: string) => void;
}) {
  // What the API added because something chosen needs it (an MQTT
  // broker for Zigbee): named here so the plan that proposes it is not
  // a surprise.
  const deps = [...comps.values()].filter((c) => c.selection === "dependency");

  if (topology === "external") {
    const conn = comps.get("home-automation");
    const install = conn?.actions.find((a) => a.kind === "install_package" && a.package);
    const pkg = install?.package ?? conn?.package ?? conn?.packages[0];
    const installed = conn?.selection === "installed";
    return (
      <div className="card ha-next">
        <h3>Next: the Home Assistant connector</h3>
        <div className="muted">
          {installed
            ? "The connector is installed. The guided setup shows what is left, including the steps in Home Assistant."
            : "The guided setup proposes the connector, and anything you chose, one plan at a time for you to approve. The connector is also an ordinary package with its own form."}
        </div>
        {conn && conn.state === "unavailable" && <div className="callout callout-warn">{conn.reason}</div>}
        <DependencyNote deps={deps} />
        <div className="row">
          <button
            type="button"
            className="btn btn-primary"
            disabled={conn?.state === "unavailable" && !installed}
            onClick={onSetup}
          >
            {installed ? "Continue setup" : "Set up the connector"} <ArrowRight size={14} aria-hidden />
          </button>
          {pkg && (
            <button type="button" className="btn btn-ghost" onClick={() => onOpenPackage(pkg)}>
              {installed ? "Open the connector" : "Use the package form instead"}
            </button>
          )}
        </div>
      </div>
    );
  }

  const chosen = draft.goals.length + (draft.bluetooth !== "none" ? 1 : 0);
  return (
    <div className="card ha-next">
      <h3>Next: check the machine is ready</h3>
      <div className="muted">
        {chosen === 0
          ? "Home Assistant on its own. You can add device types now or later."
          : "The next step checks the machine for the radios and network settings these need, before any plan is proposed."}
      </div>
      <DependencyNote deps={deps} />
      <div className="row">
        <button type="button" className="btn btn-primary" onClick={onReadiness}>
          Continue <ArrowRight size={14} aria-hidden />
        </button>
      </div>
    </div>
  );
}

function DependencyNote({ deps }: { deps: HomeAutomationComponent[] }) {
  if (deps.length === 0) return null;
  return (
    <div className="ha-deps">
      <Info size={14} aria-hidden />
      <span>Also needed: {deps.map((c) => c.title).join(", ")}.</span>
    </div>
  );
}

// ---------------------------------------------------------------- readiness

// The readiness step (#449): the chosen machine's prerequisite evidence,
// its radios, a radio picker per radio-driven component, and every
// blocker with exactly the remedies the API permits.
//
// Nothing here judges. Which checks a component needs, which radio suits
// which protocol, who owns a radio and what to do about it are all the
// API's answer (#446): this renders them, and records only what the
// person picks. A radio is picked by its stable id from the inventory;
// typing a /dev path is an explicitly advanced affordance, never the
// ordinary flow.

// Household wording for the readiness probe's checks, grouped by what
// they are for. Labels only: whether a check matters for a choice is
// the API's blockers, not this table.
const CHECK_GROUPS: { title: string; checks: { check: ReadinessCheckName; label: string }[] }[] = [
  {
    title: "Network",
    checks: [
      { check: "ipv6", label: "IPv6" },
      { check: "interface_ipv6", label: "IPv6 on the network interface" },
      { check: "multicast", label: "Multicast (device discovery)" },
      { check: "ipv6_forwarding", label: "IPv6 forwarding" },
    ],
  },
  {
    title: "Bluetooth",
    checks: [
      { check: "dbus_system_bus", label: "D-Bus system bus" },
      { check: "bluez_service", label: "BlueZ Bluetooth service" },
      { check: "bluetooth_adapter", label: "Bluetooth adapter" },
    ],
  },
  {
    title: "Thread",
    checks: [{ check: "tun", label: "TUN device (/dev/net/tun)" }],
  },
];

const KIND_LABEL: Record<RadioKind, string> = {
  zigbee: "Zigbee radio",
  zwave: "Z-Wave radio",
  thread: "Thread radio",
  bluetooth: "Bluetooth adapter",
  unknown: "Unidentified serial device",
};

// What a radio is called on screen: what it says about itself over USB,
// else its stable by-id name, else the kernel's name for it.
export function radioName(r: Radio): string {
  const product = [r.usb?.manufacturer, r.usb?.product].filter(Boolean).join(" ");
  if (product) return product;
  const byId = r.byIdPaths[0]?.split("/").pop();
  return byId || r.kernelName || r.id;
}

// How the radio is recognised again after a reboot or a replug, from the
// id's documented prefix (api.ts: serial-by-id:, usb:, sysfs:).
export function radioIdentity(id: string): { stable: boolean; text: string } {
  if (id.startsWith("serial-by-id:")) return { stable: true, text: "Recognised by its own name, in any USB port" };
  if (id.startsWith("usb:")) return { stable: true, text: "Recognised by its USB serial number, in any USB port" };
  if (id.startsWith("sysfs:"))
    return { stable: false, text: "Recognised by the USB port it is in: moving it to another port makes it a different radio" };
  return { stable: false, text: "No stable identity reported" };
}

// The components that take a radio, as the API shows them: something to
// pick from, a radio already held, or a blocker about radios. Bluetooth
// through BlueZ uses the adapter without claiming it, so it has none of
// these and gets no picker.
const RADIO_CODES = new Set(["no_radio", "radio_in_use", "radio_missing", "radio_conflict"]);
function takesRadio(c: HomeAutomationComponent): boolean {
  return (
    (c.radioCandidates?.length ?? 0) > 0 ||
    (c.radios?.length ?? 0) > 0 ||
    c.blockers.some((b) => RADIO_CODES.has(b.code) || b.actions.some((a) => a.kind === "select_radio"))
  );
}

// The radio ids a component may be given: the API's free candidates and
// any a select_radio remedy offers (a replacement for an installed one).
function radioOptions(c: HomeAutomationComponent): string[] {
  const ids = [...(c.radioCandidates ?? [])];
  for (const b of c.blockers)
    for (const a of b.actions)
      if (a.kind === "select_radio" && (!a.component || a.component === c.id))
        for (const id of a.radioIds ?? []) if (!ids.includes(id)) ids.push(id);
  return ids;
}

// The radio each component would be given, from the draft: what the
// person picked, if the API still offers it and no other component took
// it; else, when the API offers exactly one identified radio and nobody
// else picked it, that one. This is what the setup sequence (#450) sends
// as the selected radio ids. A component with a hand-typed path has no
// radio id here: the path is in `draft.manualRadios`.
export function selectedRadios(draft: Draft, status: HomeAutomationStatus): Partial<Record<HomeAutomationComponentId, string>> {
  const out: Partial<Record<HomeAutomationComponentId, string>> = {};
  const taken = new Set<string>();
  const inventory = selectedNode(status)?.radioInventory?.radios ?? [];
  const involved = status.components.filter((c) => c.selection !== "none" && takesRadio(c) && !draft.manualRadios?.[c.id]);
  // Explicit picks first, so an automatic one never takes a radio the
  // person gave to something else.
  for (const c of involved) {
    const pick = draft.radios?.[c.id];
    if (pick && radioOptions(c).includes(pick) && !taken.has(pick)) {
      out[c.id] = pick;
      taken.add(pick);
    }
  }
  for (const c of involved) {
    if (out[c.id] || draft.radios?.[c.id] || c.selection === "installed") continue;
    const detected = radioOptions(c).filter((id) =>
      inventory.some((r) => r.id === id && r.suitableFor.some((s) => s.component === c.id && s.fit === "detected")),
    );
    if (detected.length === 1 && !taken.has(detected[0])) {
      out[c.id] = detected[0];
      taken.add(detected[0]);
    }
  }
  return out;
}

export function selectedNode(status: HomeAutomationStatus): HomeAutomationNode | undefined {
  return status.nodes.find((n) => n.id === status.node);
}

// A manual path is only accepted as a device path; anything else is
// said to be wrong rather than silently stored.
export function manualPathProblem(path: string): string | undefined {
  if (!path) return undefined;
  if (!/^\/dev\/\S+$/.test(path)) return "A device path starts with /dev/ and has no spaces.";
  return undefined;
}

export type Navigate = (view: "plans" | "backups" | "nodes") => void;

export interface ActionHandlers {
  onRefresh: () => void;
  onBack: () => void;
  // Readiness: on to the setup sequence.
  onContinue?: () => void;
  // Setup: a radio remedy goes to the picker on the readiness step,
  // since the picker is not on the setup page.
  onChooseRadio?: (component?: HomeAutomationComponentId) => void;
  onOpenPackage: (pkg: string) => void;
  onOpenServices: () => void;
  onNavigate?: Navigate;
  loading: boolean;
}

function ReadinessStep({
  status,
  comps,
  draft,
  onDraft,
  handlers,
}: {
  status: HomeAutomationStatus;
  comps: Map<HomeAutomationComponentId, HomeAutomationComponent>;
  draft: Draft;
  onDraft: (d: Draft) => void;
  handlers: ActionHandlers;
}) {
  const node = selectedNode(status);
  const involved = [...comps.values()].filter((c) => c.selection !== "none");
  const picked = selectedRadios(draft, status);
  const blocking = involved.reduce((n, c) => n + c.blockers.filter((b) => b.severity === "blocking").length, 0);
  const missingPick = involved.filter(
    (c) => c.selection !== "installed" && takesRadio(c) && radioOptions(c).length > 0 && !picked[c.id] && !draft.manualRadios?.[c.id],
  );

  return (
    <section className="ha-readiness" aria-labelledby="ha-readiness-title">
      <div className="sect">
        <h2 id="ha-readiness-title" className="ha-legend">
          Is {status.node ?? "the machine"} ready?
        </h2>
        <span className="fade" />
        <span className="muted ha-checked" aria-live="polite">
          {handlers.loading ? "Checking…" : `Checked ${ago(status.generatedAt)}`}
        </span>
        <button type="button" className="btn btn-ghost" disabled={handlers.loading} onClick={handlers.onRefresh}>
          Check again
        </button>
      </div>

      <ReadinessSummary blocking={blocking} missingPick={missingPick} involved={involved.length} />

      {node ? (
        <>
          <NodeCard node={node} handlers={handlers} />
          <PrerequisiteCard node={node} involved={involved} />
          <RadioInventoryCard node={node} comps={comps} />
        </>
      ) : (
        <div className="callout callout-warn" role="note">
          No machine is chosen yet. Go back to choose which machine this runs on.
        </div>
      )}

      <h3 className="ha-legend ha-sub-head">What you chose</h3>
      <ul className="ha-list ha-readiness-list">
        {involved.map((c) => (
          <li key={c.id} id={`ha-comp-${c.id}`}>
            <div className="ha-readiness-row">
              <span className="ha-option-title">
                {c.title}
                {c.selection === "dependency" && <span className="muted"> (needed by your choices)</span>}
              </span>
              <ComponentPill c={c} />
            </div>
            <div className="muted">{c.reason}</div>
            {takesRadio(c) && node && (
              <RadioPicker c={c} node={node} comps={comps} draft={draft} picked={picked} onDraft={onDraft} />
            )}
            {c.blockers.map((b, i) => (
              <BlockerCard key={`${b.code}-${i}`} b={b} handlers={handlers} checkedAt={status.generatedAt} />
            ))}
          </li>
        ))}
      </ul>
      <div className="row">
        <button type="button" className="btn btn-secondary" onClick={handlers.onBack}>
          Back to choices
        </button>
        {handlers.onContinue && involved.length > 0 && (
          <button type="button" className="btn btn-primary" onClick={handlers.onContinue}>
            Continue to setup <ArrowRight size={14} aria-hidden />
          </button>
        )}
      </div>
    </section>
  );
}

function ReadinessSummary({
  blocking,
  missingPick,
  involved,
}: {
  blocking: number;
  missingPick: HomeAutomationComponent[];
  involved: number;
}) {
  if (involved === 0) return <div className="muted">Nothing is chosen yet.</div>;
  if (blocking > 0) {
    return (
      <div className="callout callout-bad" role="status">
        {blocking === 1 ? "One thing stands" : `${blocking} things stand`} in the way. Each one below says what was
        found and what you can do about it.
      </div>
    );
  }
  if (missingPick.length > 0) {
    return (
      <div className="callout callout-warn" role="status">
        Choose which radio to use for {missingPick.map((c) => c.title).join(" and ")}.
      </div>
    );
  }
  return (
    <div className="callout callout-ok" role="status">
      Ready: nothing stands in the way of what you chose. Nothing is installed until you approve a plan.
    </div>
  );
}

function NodeCard({ node, handlers }: { node: HomeAutomationNode; handlers: ActionHandlers }) {
  return (
    <div className="card ha-card" aria-labelledby="ha-node-title">
      <div className="ha-readiness-row">
        <h3 id="ha-node-title">The machine</h3>
        <StatusPill kind={node.connected ? "ok" : "bad"}>{node.connected ? "Connected" : "Offline"}</StatusPill>
      </div>
      <dl className="ha-facts">
        <dt>Name</dt>
        <dd>{node.id}</dd>
        {!node.connected && (
          <>
            <dt>Last seen</dt>
            <dd>{ago(node.lastSeenAt)}</dd>
          </>
        )}
        {node.agentVersion && (
          <>
            <dt>Wildstone agent</dt>
            <dd>{node.agentVersion}</dd>
          </>
        )}
        {node.hostAddress && (
          <>
            <dt>Address on your network</dt>
            <dd>{node.hostAddress}</dd>
          </>
        )}
      </dl>
      {node.blockers.map((b, i) => (
        <BlockerCard key={`${b.code}-${i}`} b={b} handlers={handlers} />
      ))}
    </div>
  );
}

// The probe's own words for a probe that did not produce evidence.
function probeProblem(node: HomeAutomationNode, probe?: string, detail?: string): string | undefined {
  switch (probe) {
    case undefined:
    case "ok":
      return undefined;
    case "offline":
      return `Not checked: ${node.id} is offline.`;
    case "agent_outdated":
      return `Not checked: the Wildstone agent on ${node.id} is too old to report this.`;
    case "not_wired":
      return "Not checked: Keystone cannot reach any machine's agent right now.";
    default:
      return `Not checked: ${node.id} did not answer${detail ? ` (${detail})` : ""}.`;
  }
}

function checkState(state: ReadinessState): string {
  switch (state) {
    case "ready":
      return "Ready";
    case "missing":
      return "Missing";
    case "unsupported":
      return "Not supported";
    default:
      return "Unknown";
  }
}

function PrerequisiteCard({ node, involved }: { node: HomeAutomationNode; involved: HomeAutomationComponent[] }) {
  const r = node.readiness;
  const problem = probeProblem(node, r?.probe, r?.detail);
  // A row is red or amber only when a component the person chose has a
  // blocker for that check (codes are <check>_<state>): the API decides
  // what matters for which choice, and a missing thing nothing chosen
  // needs is shown as a fact, not an alarm.
  const severityFor = (check: string, subject: string): "blocking" | "warning" | undefined => {
    let sev: "blocking" | "warning" | undefined;
    for (const c of involved)
      for (const b of c.blockers) {
        if (!b.code.startsWith(check + "_")) continue;
        if (b.evidence.length > 0 && !b.evidence.some((e) => e.subject === subject || e.subject === check)) continue;
        if (b.severity === "blocking") return "blocking";
        sev = "warning";
      }
    return sev;
  };
  return (
    <div className="card ha-card" aria-labelledby="ha-prereq-title">
      <h3 id="ha-prereq-title">Network and system</h3>
      {!r || problem ? (
        <div className="muted">{problem ?? `Not checked: ${node.id} was not asked.`}</div>
      ) : !r.platformSupported ? (
        <div className="muted">{node.id} cannot report these (not a Linux host).</div>
      ) : (
        <>
          {r.interfaces.length > 0 && (
            <table className="ha-table">
              <caption className="ha-caption">Network interfaces</caption>
              <thead>
                <tr>
                  <th scope="col">Interface</th>
                  <th scope="col">State</th>
                  <th scope="col">Addresses</th>
                </tr>
              </thead>
              <tbody>
                {r.interfaces.map((i) => (
                  <tr key={i.name}>
                    <th scope="row">
                      {i.name}
                      {i.defaultRoute && <span className="muted"> (default route)</span>}
                    </th>
                    <td>
                      {i.up ? "Up" : "Down"}
                      {i.multicast ? ", multicast on" : ", multicast off"}
                    </td>
                    <td className="ha-mono">{i.addresses.length ? i.addresses.join(", ") : "none"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {CHECK_GROUPS.map((g) => {
            const rows = g.checks.flatMap((c) =>
              r.checks.filter((row) => row.check === c.check).map((row) => ({ ...row, label: c.label })),
            );
            if (rows.length === 0) return null;
            return (
              <table key={g.title} className="ha-table">
                <caption className="ha-caption">{g.title}</caption>
                <tbody>
                  {rows.map((row, i) => {
                    const sev = row.state === "ready" ? undefined : severityFor(row.check, row.subject);
                    const kind: Kind =
                      row.state === "ready" ? "ok" : sev === "blocking" ? "bad" : sev === "warning" ? "warn" : "mute";
                    return (
                      <tr key={`${row.check}-${row.subject}-${i}`}>
                        <th scope="row">
                          {row.label}
                          {row.subject && row.subject !== row.check && <span className="muted ha-mono"> {row.subject}</span>}
                        </th>
                        <td>
                          <StatusPill kind={kind}>{checkState(row.state)}</StatusPill>
                          {row.state !== "ready" && !sev && <span className="muted"> not needed for what you chose</span>}
                        </td>
                        <td className="muted">{[row.value, row.detail].filter(Boolean).join(" · ")}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            );
          })}
        </>
      )}
    </div>
  );
}

function ownerLine(o: HomeAutomationRadioOwner, comps: Map<HomeAutomationComponentId, HomeAutomationComponent>): string {
  const who = (o.component && comps.get(o.component)?.title) || o.package || o.container;
  return o.package && who !== o.package ? `${who} (${o.package})` : who;
}

function RadioInventoryCard({
  node,
  comps,
}: {
  node: HomeAutomationNode;
  comps: Map<HomeAutomationComponentId, HomeAutomationComponent>;
}) {
  const inv = node.radioInventory;
  const problem = probeProblem(node, inv?.probe, inv?.detail);
  return (
    <div className="card ha-card" aria-labelledby="ha-radios-title">
      <h3 id="ha-radios-title">Radios plugged into {node.id}</h3>
      {!inv || problem ? (
        <div className="muted">{problem ?? `Not checked: ${node.id} was not asked.`}</div>
      ) : !inv.platformSupported ? (
        <div className="muted">{node.id} cannot list its radios (not a Linux host).</div>
      ) : inv.radios.length === 0 ? (
        <div className="muted">No radios found. Plug a USB radio into {node.id} and check again.</div>
      ) : (
        <ul className="ha-radios">
          {inv.radios.map((r) => (
            <RadioRow key={r.id} r={r} comps={comps} />
          ))}
        </ul>
      )}
      {inv && inv.unresolvedClaims.length > 0 && (
        <div className="callout callout-warn" role="note">
          Given to a service but not plugged in:
          <ul>
            {inv.unresolvedClaims.map((o, i) => (
              <li key={`${o.device}-${i}`}>
                <span className="ha-mono">{o.source || o.device}</span> for {ownerLine(o, comps)}
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  );
}

function RadioRow({ r, comps }: { r: HomeAutomationRadio; comps: Map<HomeAutomationComponentId, HomeAutomationComponent> }) {
  const identity = radioIdentity(r.id);
  const shared = r.owners.length > 1;
  return (
    <li className="ha-radio">
      <div className="ha-readiness-row">
        <span className="ha-option-title">{radioName(r)}</span>
        <span className="ha-pills">
          <StatusPill kind={r.kind === "unknown" ? "warn" : "mute"}>{KIND_LABEL[r.kind]}</StatusPill>
          {r.owners.length === 0 ? (
            <StatusPill kind="ok">Free</StatusPill>
          ) : (
            <StatusPill kind={shared ? "bad" : "mute"}>{shared ? `Used by ${r.owners.length} services` : "In use"}</StatusPill>
          )}
        </span>
      </div>
      <dl className="ha-facts">
        <dt>Identity</dt>
        <dd>
          {identity.text}
          {!identity.stable && <StatusPill kind="warn">Not stable</StatusPill>}
        </dd>
        {r.byIdPaths[0] && (
          <>
            <dt>Stable name</dt>
            <dd className="ha-mono">{r.byIdPaths[0].split("/").pop()}</dd>
          </>
        )}
        <dt>Suits</dt>
        <dd>
          {r.suitableFor.length === 0
            ? "Nothing you can set up here"
            : r.suitableFor
                .map((s) => `${comps.get(s.component)?.title ?? s.component} (${s.fit === "detected" ? "identified" : "possible, not identified"})`)
                .join(", ")}
        </dd>
        {r.kindReason && (
          <>
            <dt>Why</dt>
            <dd className="muted">{r.kindReason}</dd>
          </>
        )}
        {r.owners.length > 0 && (
          <>
            <dt>Used by</dt>
            <dd>{r.owners.map((o) => ownerLine(o, comps)).join(", ")}</dd>
          </>
        )}
      </dl>
      {shared && (
        <div className="callout callout-bad" role="note">
          More than one service has this radio. A radio answers one program at a time, so at most one of them works.
        </div>
      )}
    </li>
  );
}

function RadioPicker({
  c,
  node,
  comps,
  draft,
  picked,
  onDraft,
}: {
  c: HomeAutomationComponent;
  node: HomeAutomationNode;
  comps: Map<HomeAutomationComponentId, HomeAutomationComponent>;
  draft: Draft;
  picked: Partial<Record<HomeAutomationComponentId, string>>;
  onDraft: (d: Draft) => void;
}) {
  const inventory = node.radioInventory?.radios ?? [];
  const byId = new Map(inventory.map((r) => [r.id, r]));
  const options = radioOptions(c);
  const manual = draft.manualRadios?.[c.id] ?? "";
  const [advanced, setAdvanced] = useState(manual !== "");
  const current = picked[c.id];
  const wanted = draft.radios?.[c.id];
  // Suitable radios someone else holds: shown so the person sees why
  // they are not offered; freeing one is the owning service's business.
  const held = inventory.filter(
    (r) =>
      !options.includes(r.id) &&
      r.owners.length > 0 &&
      r.suitableFor.some((s) => s.component === c.id) &&
      !r.owners.some((o) => o.component === c.id),
  );
  const pickedElsewhere = (id: string) =>
    (Object.entries(picked) as [HomeAutomationComponentId, string][]).find(([k, v]) => v === id && k !== c.id)?.[0];
  const setRadio = (id: string) =>
    onDraft({ ...draft, radios: { ...draft.radios, [c.id]: id } });
  const setManual = (path: string) => {
    const next = { ...draft.manualRadios };
    if (path) next[c.id] = path;
    else delete next[c.id];
    onDraft({ ...draft, manualRadios: next });
  };
  const fieldId = `ha-radio-${c.id}`;
  const manualId = `ha-manual-${c.id}`;
  const manualProblem = manualPathProblem(manual);
  const installed = c.selection === "installed";

  return (
    <div className="ha-picker">
      {installed && (c.radios?.length ?? 0) > 0 && (
        <div className="ha-current">
          Using{" "}
          {c.radios!.map((o, i) => {
            const r = o.radioId ? byId.get(o.radioId) : undefined;
            return (
              <span key={`${o.device}-${i}`}>
                {i > 0 && ", "}
                <strong>{r ? radioName(r) : o.source || o.device}</strong>
                {!o.radioId && <span className="muted"> (not plugged in)</span>}
              </span>
            );
          })}
          .
        </div>
      )}
      {(!installed || options.length > 0) && (
        <fieldset className="ha-fieldset ha-nested" id={fieldId} tabIndex={-1}>
          <legend className="ha-legend-sub">{installed ? `Replacement radio for ${c.title}` : `Radio for ${c.title}`}</legend>
          {options.length === 0 && held.length === 0 && (
            <div className="muted">No radio on {node.id} suits {c.title}.</div>
          )}
          {wanted && !options.includes(wanted) && !manual && (
            <div className="callout callout-warn" role="note">
              The radio you chose earlier is no longer offered for {c.title}: it was unplugged or another service took it.
            </div>
          )}
          <div className="ha-options">
            {options.map((id) => {
              const r = byId.get(id);
              const fit = r?.suitableFor.find((s) => s.component === c.id)?.fit;
              const other = pickedElsewhere(id);
              const on = current === id && !manual;
              const oid = `${fieldId}-${id.replace(/[^A-Za-z0-9_-]/g, "_")}`;
              return (
                <label key={id} className={on ? "ha-option on" : "ha-option"}>
                  <input
                    type="radio"
                    name={fieldId}
                    value={id}
                    checked={on}
                    disabled={!!other || !!manual}
                    onChange={() => setRadio(id)}
                    aria-labelledby={`${oid}-title`}
                    aria-describedby={`${oid}-desc`}
                  />
                  <span className="ha-option-body">
                    <span id={`${oid}-title`} className="ha-option-title">
                      {r ? radioName(r) : id}
                    </span>
                    <span id={`${oid}-desc`} className="ha-option-text">
                      {r ? KIND_LABEL[r.kind] : "Not in the latest inventory"}
                      {fit === "possible" &&
                        ` — could be a ${c.title} radio, but it did not say so. Choose it only if you know it is one.`}
                      {r && ` ${radioIdentity(r.id).text}.`}
                      {other && ` Chosen for ${comps.get(other)?.title ?? other}.`}
                    </span>
                    {on && !wanted && <StatusPill kind="mute">The only identified radio, chosen for you</StatusPill>}
                  </span>
                </label>
              );
            })}
            {held.map((r) => (
              <div key={r.id} className="ha-option ha-option-off" aria-disabled="true">
                <span className="ha-option-body">
                  <span className="ha-option-title">{radioName(r)}</span>
                  <span className="ha-option-text">
                    Used by {r.owners.map((o) => ownerLine(o, comps)).join(", ")}. Free it from that service to use it
                    here.
                  </span>
                </span>
              </div>
            ))}
          </div>
          <div className="ha-manual">
            <button
              type="button"
              className="btn btn-ghost btn-sm"
              aria-expanded={advanced}
              aria-controls={`${manualId}-panel`}
              onClick={() => setAdvanced(!advanced)}
            >
              Advanced: enter a device path by hand
            </button>
            {advanced && (
              <div id={`${manualId}-panel`} className="ha-manual-panel">
                <label htmlFor={manualId} className="ha-legend-sub">
                  Device path for {c.title}
                </label>
                <div className="muted ha-hint-inline">
                  Only for a radio the list above does not show. A typed path is not checked against the inventory, and
                  it is used instead of a radio chosen above.
                </div>
                <input
                  id={manualId}
                  className="input ha-mono"
                  type="text"
                  spellCheck={false}
                  autoComplete="off"
                  placeholder="/dev/serial/by-id/…"
                  value={manual}
                  aria-invalid={manualProblem ? true : undefined}
                  aria-describedby={manualProblem ? `${manualId}-err` : undefined}
                  onChange={(e) => setManual(e.target.value.trim())}
                />
                {manualProblem && (
                  <div id={`${manualId}-err`} className="ha-field-err" role="alert">
                    {manualProblem}
                  </div>
                )}
              </div>
            )}
          </div>
        </fieldset>
      )}
    </div>
  );
}

// One blocker: what was found (kept on screen through every "Check
// again" until the evidence changes), and the remedies the API permits.
export function BlockerCard({
  b,
  handlers,
  checkedAt,
}: {
  b: HomeAutomationBlocker;
  handlers: ActionHandlers;
  checkedAt?: string;
}) {
  const seen = new Set<string>();
  const actions = b.actions.filter((a) => {
    const k = `${a.kind}|${a.label}|${a.package ?? ""}|${a.component ?? ""}`;
    if (seen.has(k)) return false;
    seen.add(k);
    return true;
  });
  const manual = actions.filter((a) => a.kind === "manual_step" && (a.instructions?.length ?? 0) > 0);
  const buttons = actions.filter((a) => !(a.kind === "manual_step" && (a.instructions?.length ?? 0) > 0));
  return (
    <div className={b.severity === "blocking" ? "callout callout-bad ha-blocker" : "callout callout-warn ha-blocker"} role="note">
      <div className="ha-blocker-msg">{b.message}</div>
      {b.evidence.length > 0 && (
        <ul className="ha-evidence" aria-label="What was found">
          {b.evidence.map((e, i) => (
            <li key={i}>{evidenceLine(e)}</li>
          ))}
        </ul>
      )}
      {manual.map((a, i) => (
        <div key={`m-${i}`} className="ha-manual-step">
          <div className="ha-legend-sub">{a.label}{a.node ? ` on ${a.node}` : ""}</div>
          <ol>
            {a.instructions!.map((line, j) => (
              <li key={j}>{line}</li>
            ))}
          </ol>
        </div>
      ))}
      {buttons.length > 0 && (
        <div className="row ha-actions">
          {buttons.map((a, i) => (
            <ActionButton key={i} a={a} handlers={handlers} />
          ))}
        </div>
      )}
      {checkedAt && actions.some((a) => a.kind === "retry_probe") && (
        <div className="muted ha-checked">{handlers.loading ? "Checking…" : `Last checked ${ago(checkedAt)}`}</div>
      )}
    </div>
  );
}

export function evidenceLine(e: HomeAutomationEvidence): string {
  const head = [e.subject, e.state].filter(Boolean).join(": ");
  const tail = [e.value, e.detail].filter(Boolean).join(" · ");
  return [head || e.source, tail].filter(Boolean).join(" — ");
}

function ActionButton({ a, handlers }: { a: HomeAutomationAction; handlers: ActionHandlers }) {
  const btn = (onClick: () => void, disabled = false) => (
    <button type="button" className="btn btn-secondary btn-sm" disabled={disabled} onClick={onClick}>
      {a.label}
    </button>
  );
  // In-page targets are links: the picker or component card is already
  // on this screen.
  const jump = (href: string) => (
    <a className="btn btn-secondary btn-sm" href={href}>
      {a.label}
    </a>
  );
  switch (a.kind) {
    case "retry_probe":
      return btn(handlers.onRefresh, handlers.loading);
    case "select_radio":
      if (handlers.onChooseRadio) return btn(() => handlers.onChooseRadio!(a.component));
      return a.component ? jump(`#ha-radio-${a.component}`) : null;
    case "choose_mode":
    case "provide_option":
      // Answered by the setup step's own fields under the blocker.
      return null;
    case "resolve_dependency":
      return a.component ? jump(`#ha-comp-${a.component}`) : null;
    case "choose_node":
      return btn(handlers.onBack);
    case "install_package":
    case "reconfigure_package":
    case "upgrade_package":
    case "open_service":
    case "mint_publisher":
      return a.package ? btn(() => handlers.onOpenPackage(a.package!)) : btn(handlers.onOpenServices);
    case "open_plan":
      return handlers.onNavigate ? btn(() => handlers.onNavigate!("plans")) : null;
    case "open_backups":
      return handlers.onNavigate ? btn(() => handlers.onNavigate!("backups")) : null;
    case "pair_node":
      return handlers.onNavigate ? btn(() => handlers.onNavigate!("nodes")) : null;
    case "manual_step":
      // A manual step without instructions has nothing to show but its
      // label; it is followed by the Check again beside it.
      return <span className="ha-manual-label">{a.label}</span>;
  }
  return null;
}
