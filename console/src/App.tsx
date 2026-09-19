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
  Repeat,
  CalendarCheck,
  SealCheck,
  ShieldCheck,
  Sparkle,
  SquaresFour,
  UsersThree,
} from "@phosphor-icons/react";
import { api, clearToken, getToken, Identity, setOnStepUp, setOnUnauthorized, setToken } from "./api";
import { Confirm } from "./ui/Confirm";
import { Toast, useToast } from "./ui/Toast";
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
import { Integrations } from "./views/Integrations";
import { IdentityView } from "./views/Identity";
import { Memory } from "./views/Memory";
import { Nodes } from "./views/Nodes";
import { Plans } from "./views/Plans";
import { Butlers } from "./views/Butlers";
import { Commitments } from "./views/Commitments";
import { Routines } from "./views/Routines";
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
  | "integrations"
  | "routines"
  | "commitments"
  | "butlers"
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
  integrations: ["Connected apps", "Integrations"],
  routines: ["On its own", "Routines"],
  commitments: ["The house keeps track", "Commitments"],
  butlers: ["Who answers", "Butlers"],
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
  // null while we find out. With no token of our own, the identity
  // proxy may already have signed this person in (ADR-W032): ask the
  // server who we are before offering the token gate, which is the way
  // in only when the proxy is not the one asking.
  const [authed, setAuthed] = useState<boolean | null>(getToken() !== null ? true : null);

  useEffect(() => {
    setOnUnauthorized(() => setAuthed(false));
    if (getToken() === null) {
      api
        .whoami()
        .then(() => setAuthed(true))
        .catch(() => setAuthed(false));
    }
  }, []);

  if (authed === null) {
    return null;
  }
  if (!authed) {
    return <TokenGate onDone={() => setAuthed(true)} />;
  }
  return (
    <ConsoleStateProvider>
      <Shell />
      <StepUp />
    </ConsoleStateProvider>
  );
}

export function Shell() {
  const [who, setWho] = useState<Identity | null>(null);
  const [view, setView] = useState<View>("home");
  // Which routine another screen asked to open. Held here because the
  // Routines screen keeps its own mode and cannot be told from outside
  // once it is mounted.
  const [openRoutine, setOpenRoutine] = useState<string | undefined>();
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
      { id: "butlers", label: "Butlers", icon: <UsersThree size={ICON} /> },
      { id: "work", label: "Work", icon: <ListChecks size={ICON} /> },
      { id: "authority", label: "Butler access", icon: <ShieldCheck size={ICON} /> },
      { id: "memory", label: "Memory", icon: <Brain size={ICON} /> },
      { id: "integrations", label: "Integrations", icon: <PlugsConnected size={ICON} /> },
      { id: "routines", label: "Routines", icon: <Repeat size={ICON} /> },
      { id: "commitments", label: "Commitments", icon: <CalendarCheck size={ICON} /> },
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
        // Choosing Routines from the rail means the list, even if
        // something else asked to open one routine a moment ago.
        onSelect={(to) => {
          setOpenRoutine(undefined);
          setView(to);
        }}
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
            <SignOut />
          </div>
        </div>
        <div className={FULL_BLEED.has(view) ? "screen screen-flush" : "screen"}>
          <ErrorLine error={error} />
          {view === "home" && <Home onNavigate={setView} />}
          {view === "services" && <Services />}
          {view === "butler" && <Butler onEyebrow={setEyebrowOverride} onNavigate={setView} />}
          {view === "work" && <Work />}
          {view === "authority" && <ButlerAuthority />}
          {view === "memory" && <Memory />}
          {view === "integrations" && <Integrations />}
          {view === "routines" && <Routines key={openRoutine ?? "list"} openRoutine={openRoutine} />}
          {view === "commitments" && <Commitments />}
          {view === "butlers" && <Butlers />}
          {view === "chat" && <Chat />}
          {view === "household" && <Household />}
          {view === "connect" && <ConnectChat />}
          {view === "plans" && <Plans onNavigate={setView} />}
          {view === "activity" && (
            <Activity
              onOpenRoutine={(name) => {
                setOpenRoutine(name);
                setView("routines");
              }}
            />
          )}
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

// TokenGate (ADR-W016 A3): the way in when the identity proxy is not
// the one asking. The token is verified with whoami before it is kept,
// and kept only in sessionStorage.
//
// It says BOTH ways now, because there are two and this screen only
// ever admitted to one. The ordinary way in is the account the installer
// created: an Owner signs in through the identity proxy, which asserts
// who they are and never reaches this page at all. A token is the other
// way — a deployment with no ingress yet, a recovery session, a script.
//
// Naming only the token made the Console look like a machine surface
// with a password-shaped hole, and left an operator who had just chosen
// a username and password at install with nothing here that matched what
// they had been given (design §2.4).
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
        <div className="muted" style={{ marginBottom: "var(--space-3)" }}>
          If your household has a sign-in page, use it — you reach the Console as
          yourself, with the username and password you chose when Wildstone was
          installed. You will not see this screen.
        </div>
        <div className="muted" style={{ marginBottom: "var(--space-4)" }}>
          Otherwise, paste an API token (first start printed one; mint more with
          wild).
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

// SignOut leaves the Console (ADR-W033 A4.1).
//
// Signed in through the proxy, it ends the Console session and then the
// household one at the identity provider: leaving only the Console would
// sign the person straight back in on their next visit. With a token it
// forgets the token, which is all a token sign-in ever was.
function SignOut() {
  const [busy, setBusy] = useState(false);
  const leave = async () => {
    setBusy(true);
    if (getToken() !== null) {
      clearToken();
      location.assign("/");
      return;
    }
    const done = await api.consoleSignout().catch(() => ({}) as { signOut?: string });
    location.assign(done.signOut ?? "/");
  };
  return (
    <button className="btn btn-ghost" onClick={leave} disabled={busy}>
      Sign out
    </button>
  );
}


const STEPPED_UP_KEY = "wst.console.stepped-up";

// StepUp is the Console's half of ADR-W033 A4.1's consequential gate.
//
// A drastic operation refused because the sign-in behind it is more than
// a few minutes old opens this: it says why, and sends the person to
// confirm their password with the identity provider. They come back to
// the same screen and press the button again. Nothing is replayed for
// them: a write re-sent after a redirect is one they did not see
// themselves make.
function StepUp() {
  const [url, setUrl] = useState<string | null>(null);
  const { toast, say } = useToast();
  useEffect(() => {
    setOnStepUp((u) => setUrl(u));
    if (sessionStorage.getItem(STEPPED_UP_KEY)) {
      sessionStorage.removeItem(STEPPED_UP_KEY);
      say("Password confirmed. Press the button again to finish.");
    }
  }, [say]);
  return (
    <>
      {url && (
        <Confirm
          kind="warn"
          icon={<ShieldCheck size={18} weight="fill" />}
          title="Confirm your password"
          body={
            "This can't easily be undone, so the Console asks you to sign in again first. " +
            "You'll come back to this screen; press the button again to finish."
          }
          confirmLabel="Confirm password"
          cancelLabel="Not now"
          onConfirm={() => {
            sessionStorage.setItem(STEPPED_UP_KEY, "1");
            const here = location.pathname + location.search + location.hash;
            location.assign(`${url}&return=${encodeURIComponent(here)}`);
          }}
          onCancel={() => setUrl(null)}
        />
      )}
      <Toast toast={toast} />
    </>
  );
}
