import { useEffect, useMemo, useState } from "react";
import {
  Archive,
  Brain,
  ChatsCircle,
  Cpu,
  House,
  IdentificationCard,
  Key,
  ListChecks,
  PlugsConnected,
  Pulse,
  SealCheck,
  ShieldCheck,
  Sparkle,
  SquaresFour,
  UsersThree,
} from "@phosphor-icons/react";
import { api, getToken, Identity, setOnUnauthorized, setToken } from "./api";
import { ago, ErrorLine } from "./lib";
import { ConsoleStateProvider, useConsoleState } from "./state";
import { Dot, Kind, StatusPill } from "./ui/status";
import { Rail, RailItem } from "./ui/Rail";
import { Activity } from "./views/Activity";
import { Backups } from "./views/Backups";
import { Butler } from "./views/Butler";
import { ButlerAuthority } from "./views/ButlerAuthority";
import { Chat } from "./views/Chat";
import { ConnectChat } from "./views/ConnectChat";
import { Home } from "./views/Home";
import { Household } from "./views/Household";
import { IdentityView } from "./views/Identity";
import { Memory } from "./views/Memory";
import { Nodes } from "./views/Nodes";
import { Plans } from "./views/Plans";
import { Secrets } from "./views/Secrets";
import { Work } from "./views/Work";
import { Services } from "./views/Services";

type View =
  | "home"
  | "services"
  | "butler"
  | "work"
  | "authority"
  | "memory"
  | "chat"
  | "household"
  | "connect"
  | "plans"
  | "activity"
  | "backups"
  | "nodes"
  | "secrets"
  | "identity";

// The eyebrow says what kind of thing the screen is; the title names
// it. Both come from the design, and the pair is what tells you where
// you are once the nav is icons.
//
// Home's eyebrow is "Overview" rather than "Household", which is what
// the design called it: there is a screen named Household now, and two
// different things under one word is worse than a duller word.
const TITLES: Record<View, [string, string]> = {
  home: ["Overview", "Home"],
  services: ["Catalog", "Services"],
  butler: ["Operator", "Butler"],
  work: ["Durable", "Work"],
  authority: ["Admission", "Butler access"],
  memory: ["Owned data", "Memory"],
  chat: ["Access", "Chat"],
  household: ["People", "Household"],
  connect: ["Setup", "Connect chat"],
  plans: ["Approvals", "Plans"],
  activity: ["Stream", "Activity"],
  backups: ["Standing duty", "Backups"],
  nodes: ["Fleet", "Nodes"],
  secrets: ["Write-only", "Secrets"],
  identity: ["Out-of-band", "Identity"],
};

const ICON = 19;

// Screens that own the viewport instead of scrolling inside the padded,
// capped .screen box. The Butler is a conversation: its header, thread
// and composer are three fixed bands, and only the middle one scrolls.
const FULL_BLEED: ReadonlySet<View> = new Set<View>(["butler"]);

export function App() {
  const [authed, setAuthed] = useState(getToken() !== null);

  useEffect(() => {
    setOnUnauthorized(() => setAuthed(false));
  }, []);

  if (!authed) {
    return <TokenGate onDone={() => setAuthed(true)} />;
  }
  return (
    <ConsoleStateProvider>
      <Shell />
    </ConsoleStateProvider>
  );
}

export function Shell() {
  const [who, setWho] = useState<Identity | null>(null);
  const [view, setView] = useState<View>("home");
  const { snap, error } = useConsoleState();

  useEffect(() => {
    api.whoami().then(setWho).catch(() => {});
  }, []);

  // Pips are the whole point of an icon rail: they say which screen has
  // something for you without your having to visit it. No pip is the
  // resting state, and a healthy console shows none at all.
  const items = useMemo((): RailItem<View>[] => {
    const homePip: Kind | undefined = snap?.unhealthy.length
      ? "bad"
      : snap?.awaiting.length
        ? "warn"
        : undefined;
    const plansPip: Kind | undefined = snap?.pending.length ? "warn" : undefined;
    const nodesPip: Kind | undefined = snap?.nodes.some((n) => !n.connected) ? "bad" : undefined;
    return [
      { id: "home", label: "Home", icon: <House size={ICON} />, pip: homePip },
      { id: "services", label: "Services", icon: <SquaresFour size={ICON} /> },
      { id: "butler", label: "Butler", icon: <Sparkle size={ICON} /> },
      { id: "work", label: "Work", icon: <ListChecks size={ICON} /> },
      { id: "authority", label: "Butler access", icon: <ShieldCheck size={ICON} /> },
      { id: "memory", label: "Memory", icon: <Brain size={ICON} /> },
      { id: "chat", label: "Chat", icon: <ChatsCircle size={ICON} /> },
      { id: "household", label: "Household", icon: <UsersThree size={ICON} /> },
      { id: "connect", label: "Connect chat", icon: <PlugsConnected size={ICON} /> },
      { id: "plans", label: "Plans", icon: <SealCheck size={ICON} />, pip: plansPip },
      { id: "activity", label: "Activity", icon: <Pulse size={ICON} /> },
      { id: "backups", label: "Backups", icon: <Archive size={ICON} /> },
      { id: "nodes", label: "Nodes", icon: <Cpu size={ICON} />, pip: nodesPip },
      { id: "secrets", label: "Secrets", icon: <Key size={ICON} />, dividerBefore: true },
      { id: "identity", label: "Identity", icon: <IdentificationCard size={ICON} /> },
    ];
  }, [snap]);

  const [eyebrow, title] = TITLES[view];
  // A screen may override the eyebrow: the Butler's says which SURFACE
  // is open, and a bar reading "OPERATOR" over the household butler
  // would be teaching the wrong thing.
  const [eyebrowOverride, setEyebrowOverride] = useState<string | null>(null);

  return (
    <div className="shell">
      <Rail
        items={items}
        current={view}
        onSelect={setView}
        mark={<ShieldCheck size={22} weight="fill" />}
        footer={
          who && (
            <div className="avatar" title={`${who.name} · ${who.role}`}>
              {initials(who.name)}
            </div>
          )
        }
      />
      <main className="main">
        <div className="topbar">
          <span className="eyebrow">{(view === "butler" && eyebrowOverride) || eyebrow}</span>
          <span className="screen-title">{title}</span>
          <div className="meta">
            <Freshness />
          </div>
        </div>
        <div className={FULL_BLEED.has(view) ? "screen screen-flush" : "screen"}>
          <ErrorLine error={error} />
          {view === "home" && <Home onNavigate={setView} />}
          {view === "services" && <Services />}
          {view === "butler" && <Butler onEyebrow={setEyebrowOverride} />}
          {view === "work" && <Work />}
          {view === "authority" && <ButlerAuthority />}
          {view === "memory" && <Memory />}
          {view === "chat" && <Chat />}
          {view === "household" && <Household />}
          {view === "connect" && <ConnectChat />}
          {view === "plans" && <Plans onNavigate={setView} />}
          {view === "activity" && <Activity />}
          {view === "backups" && <Backups />}
          {view === "nodes" && <Nodes />}
          {view === "secrets" && <Secrets />}
          {view === "identity" && <IdentityView />}
        </div>
      </main>
    </div>
  );
}

// What the header can honestly say today. The statefile head comes from
// the newest plan's basedOnHead and the reconcile age from the newest
// reconcile.tick — both already on the wire. A GET /status would serve
// them directly, and this block is what it would replace.
export function Freshness() {
  const { snap, error } = useConsoleState();
  if (!snap) return null;

  const head = snap.plans.find((p) => p.basedOnHead)?.basedOnHead;
  // Any reconcile outcome dates the last pass — applied, blocked,
  // failed or skipped. Matching one exact type is how this went blank
  // the first time.
  const tick = [...snap.events].reverse().find((e) => e.type.startsWith("reconcile."));
  const live: Kind = error ? "bad" : "ok";

  return (
    <>
      {head && (
        <span className="tnum" title="statefile head the newest plan was based on">
          statefile {head.slice(0, 6)}
        </span>
      )}
      {tick && <span title={tick.time}>reconciled {ago(tick.time)}</span>}
      <StatusPill kind={live} icon={<Dot kind={live} pulse={!error} />}>
        {error ? "stale" : "live"}
      </StatusPill>
    </>
  );
}

function initials(name: string): string {
  const parts = name.trim().split(/[\s._-]+/).filter(Boolean);
  if (parts.length === 0) return "?";
  if (parts.length === 1) return parts[0].slice(0, 2).toLowerCase();
  return (parts[0][0] + parts[1][0]).toLowerCase();
}

// TokenGate (ADR-W016 A3): paste an API token; verified with whoami
// before it is kept, and kept only in sessionStorage.
export function TokenGate({ onDone }: { onDone: () => void }) {
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
        <div style={{ display: "flex", alignItems: "center", gap: "var(--space-3)", marginBottom: "var(--space-2)" }}>
          <ShieldCheck size={22} weight="fill" color="var(--color-accent)" />
          <h3 style={{ margin: 0 }}>Wildstone Console</h3>
        </div>
        <div className="muted" style={{ marginBottom: "var(--space-4)" }}>
          Paste an API token (first start printed one; mint more with wild).
        </div>
        <div className="row">
          <input
            type="password"
            placeholder="wst_…"
            aria-label="API token"
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && input && submit()}
          />
        </div>
        <button className="btn btn-primary btn-block" disabled={busy || !input} onClick={submit}>
          {busy ? "Checking…" : "Enter"}
        </button>
        <ErrorLine error={error} />
      </div>
    </div>
  );
}
