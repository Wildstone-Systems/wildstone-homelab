import { useEffect, useMemo, useState } from "react";
import { ArrowRight, Info, SquaresFour } from "@phosphor-icons/react";
import {
  api,
  ApiError,
  BluetoothMode,
  HomeAutomationComponent,
  HomeAutomationComponentId,
  HomeAutomationQuery,
  HomeAutomationStatus,
  HomeAutomationTopology,
} from "../api";
import { ago, ErrorLine, useLoad } from "../lib";
import { Kind, StatusPill } from "../ui/status";

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
// evidence and radio picker land (#449); the setup sequence (#450) and
// the composed status page (#451) follow it.

export type Step = "choose" | "readiness";

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
export interface Draft {
  topology?: HomeAutomationTopology;
  node?: string;
  goals: Goal[];
  bluetooth: BluetoothChoice;
  step: Step;
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
    const step: Step = r.step === "readiness" && topology === "managed" ? "readiness" : "choose";
    const node = typeof r.node === "string" && r.node ? r.node : undefined;
    return { topology, node, goals, bluetooth, step };
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
}: {
  onOpenPackage: (pkg: string) => void;
  onOpenServices: () => void;
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
}: {
  status: HomeAutomationStatus;
  draft: Draft;
  onDraft: (d: Draft) => void;
  loading?: boolean;
  error?: string | null;
  onRefresh?: () => void;
  onOpenPackage: (pkg: string) => void;
  onOpenServices: () => void;
}) {
  const topology = effectiveTopology(draft, status);
  const comps = useMemo(() => new Map(status.components.map((c) => [c.id, c])), [status.components]);

  return (
    <div className="ha">
      <div className="sub">
        Run Home Assistant on one of your machines, or connect the one you already have, and choose the
        kinds of devices you want it to reach. Choices here are only choices: nothing is installed until you
        approve a plan.
      </div>
      <ErrorLine error={error} />
      <InstalledNow status={status} />
      {status.blockers.map((b) => (
        <div key={b.code} className={b.severity === "blocking" ? "callout callout-bad" : "callout callout-warn"} role="note">
          {b.message}
        </div>
      ))}

      {draft.step === "readiness" && topology === "managed" ? (
        <ReadinessStep
          status={status}
          comps={comps}
          onBack={() => onDraft({ ...draft, step: "choose" })}
          onRefresh={onRefresh}
          loading={loading}
        />
      ) : (
        <>
          <TopologyChoice
            value={topology}
            comps={comps}
            onChange={(t) => onDraft({ ...draft, topology: t, step: "choose" })}
          />
          {topology && (
            <>
              <NodeChoice status={status} value={draft.node} onChange={(n) => onDraft({ ...draft, node: n })} />
              <GoalChoice draft={draft} topology={topology} comps={comps} onDraft={onDraft} />
              <NextStep
                topology={topology}
                draft={draft}
                comps={comps}
                onReadiness={() => onDraft({ ...draft, topology, step: "readiness" })}
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

function ComponentPill({ c }: { c: HomeAutomationComponent }) {
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
  onOpenPackage,
}: {
  topology: HomeAutomationTopology;
  draft: Draft;
  comps: Map<HomeAutomationComponentId, HomeAutomationComponent>;
  onReadiness: () => void;
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
            ? "The connector is installed. Its settings and the steps left in Home Assistant are on its service page."
            : "The connector is an ordinary package: its form asks for this machine and where your Home Assistant is, and shows you the plan before anything changes."}
        </div>
        {conn && conn.state === "unavailable" && <div className="callout callout-warn">{conn.reason}</div>}
        <DependencyNote deps={deps} />
        <div className="row">
          <button
            type="button"
            className="btn btn-primary"
            disabled={!pkg || conn?.state === "unavailable"}
            onClick={() => pkg && onOpenPackage(pkg)}
          >
            {installed ? "Open the connector" : "Set up the connector"} <ArrowRight size={14} aria-hidden />
          </button>
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

// The readiness step. #449 fills it with the prerequisite evidence, the
// radio picker and each blocker's remedy; until then it shows the API's
// own per-component answer so the person can see where they stand.
function ReadinessStep({
  status,
  comps,
  onBack,
  onRefresh,
  loading,
}: {
  status: HomeAutomationStatus;
  comps: Map<HomeAutomationComponentId, HomeAutomationComponent>;
  onBack: () => void;
  onRefresh: () => void;
  loading: boolean;
}) {
  const involved = [...comps.values()].filter((c) => c.selection !== "none");
  return (
    <section className="ha-readiness" aria-labelledby="ha-readiness-title">
      <div className="sect">
        <h2 id="ha-readiness-title" className="ha-legend">
          Is {status.node ?? "the machine"} ready?
        </h2>
        <span className="fade" />
        <button type="button" className="btn btn-ghost" disabled={loading} onClick={onRefresh}>
          Check again
        </button>
      </div>
      <ul className="ha-list ha-readiness-list">
        {involved.map((c) => (
          <li key={c.id}>
            <div className="ha-readiness-row">
              <span className="ha-option-title">{c.title}</span>
              <ComponentPill c={c} />
            </div>
            <div className="muted">{c.reason}</div>
            {c.blockers.map((b, i) => (
              <div key={`${b.code}-${i}`} className={b.severity === "blocking" ? "callout callout-bad" : "callout callout-warn"}>
                {b.message}
              </div>
            ))}
          </li>
        ))}
      </ul>
      <div className="row">
        <button type="button" className="btn btn-secondary" onClick={onBack}>
          Back to choices
        </button>
      </div>
    </section>
  );
}
