import { useEffect, useMemo, useState } from "react";
import {
  Archive,
  Brain,
  ChatsCircle,
  Cpu,
  House,
  IdentificationCard,
  Lightbulb,
  Key,
  ListChecks,
  PlugsConnected,
  Pulse,
  Repeat,
  CalendarCheck,
  EnvelopeSimple,
  SealCheck,
  ShieldCheck,
  Sparkle,
  Sun,
  SquaresFour,
  UsersThree,
} from "@phosphor-icons/react";
import {
  api,
  clearStepUp,
  Identity,
  pendingStepUp,
  rememberStepUp,
  setOnStepUp,
  setOnUnauthorized,
  signInURL,
} from "./api";
import { Confirm } from "./ui/Confirm";
import { ago, ErrorLine } from "./lib";
import { ConsoleStateProvider, useConsoleState } from "./state";
import { Dot, Kind, StatusPill } from "./ui/status";
import { Rail, RailItem } from "./ui/Rail";
import { ThemePicker } from "./ui/ThemePicker";
import { adopt } from "./theme";
import { Activity } from "./views/Activity";
import { Backups } from "./views/Backups";
import { Butler } from "./views/Butler";
import { ButlerAuthority } from "./views/ButlerAuthority";
import { Chat } from "./views/Chat";
import { Home } from "./views/Home";
import { HomeAutomation } from "./views/HomeAutomation";
import { Household } from "./views/Household";
import { Integrations } from "./views/Integrations";
import { MailWatch } from "./views/MailWatch";
import { IdentityView } from "./views/Identity";
import { Memory } from "./views/Memory";
import { MorningReport } from "./views/MorningReport";
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
  | "homeAutomation"
  | "butler"
  | "work"
  | "authority"
  | "memory"
  | "integrations"
  | "mail"
  | "routines"
  | "commitments"
  | "butlers"
  | "chat"
  | "household"
  | "plans"
  | "activity"
  | "report"
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
  homeAutomation: ["Setup", "Home automation"],
  butler: ["Operator", "Butler"],
  work: ["Durable", "Work"],
  authority: ["Admission", "Butler access"],
  memory: ["Owned data", "Memory"],
  integrations: ["Connected apps", "Integrations"],
  mail: ["Watch this mailbox", "Mailbox"],
  routines: ["On its own", "Routines"],
  commitments: ["The house keeps track", "Commitments"],
  butlers: ["Who answers", "Butlers"],
  chat: ["Access", "Chat"],
  household: ["People", "Household"],
  plans: ["Approvals", "Plans"],
  activity: ["Stream", "Activity"],
  report: ["Daily", "Morning report"],
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
  // null while we find out. The only way in is a Console session from
  // the identity provider's sign-in (ADR-W033 A4, A4.1): ask the server
  // who we are, and a 401 that can be fixed by signing in sends the
  // browser round it (api.ts). Anything else lands on SignIn.
  const [authed, setAuthed] = useState<boolean | null>(null);

  useEffect(() => {
    setOnUnauthorized(() => setAuthed(false));
    api
      .whoami()
      .then(() => setAuthed(true))
      .catch(() => setAuthed(false));
  }, []);

  if (authed === null) {
    return null;
  }
  if (!authed) {
    return <SignIn />;
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
  // Which package another screen asked Services to open — the Home
  // Automation flow hands off to the ordinary install form rather than
  // growing a second one. Held here for the same reason as openRoutine.
  const [openPackage, setOpenPackage] = useState<string | undefined>();
  const { snap, error } = useConsoleState();

  useEffect(() => {
    api.whoami().then(setWho).catch(() => {});
    // The theme this person chose, which the cached block already
    // guessed at before React mounted (theme.ts). Corrected here on
    // every load, so a choice made on the household dashboard — or on
    // another machine — is showing by the time the shell paints.
    api.appearance().then(adopt).catch(() => {});
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
      { id: "homeAutomation", label: "Home automation", icon: <Lightbulb size={ICON} /> },
      { id: "butler", label: "Butler", icon: <Sparkle size={ICON} /> },
      { id: "butlers", label: "Butlers", icon: <UsersThree size={ICON} /> },
      { id: "work", label: "Work", icon: <ListChecks size={ICON} /> },
      { id: "authority", label: "Butler access", icon: <ShieldCheck size={ICON} /> },
      { id: "memory", label: "Memory", icon: <Brain size={ICON} /> },
      { id: "integrations", label: "Integrations", icon: <PlugsConnected size={ICON} /> },
      { id: "mail", label: "Mailbox", icon: <EnvelopeSimple size={ICON} /> },
      { id: "routines", label: "Routines", icon: <Repeat size={ICON} /> },
      { id: "commitments", label: "Commitments", icon: <CalendarCheck size={ICON} /> },
      { id: "chat", label: "Chat", icon: <ChatsCircle size={ICON} /> },
      { id: "household", label: "Household", icon: <UsersThree size={ICON} /> },
      { id: "plans", label: "Plans", icon: <SealCheck size={ICON} />, pip: plansPip },
      { id: "activity", label: "Activity", icon: <Pulse size={ICON} /> },
      { id: "report", label: "Morning report", icon: <Sun size={ICON} /> },
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
          setOpenPackage(undefined);
          setView(to);
        }}
        mark={<ShieldCheck size={22} weight="fill" />}
        footer={
          <>
            {/* Adding a theme changes what the whole household can
                choose from, so it is offered to the role that
                administers the deployment. Choosing one is offered to
                everybody, because it is nobody else's business. */}
            <ThemePicker canImport={who?.role === "admin"} />
            {who && (
              <div className="avatar" title={`${who.name} · ${who.role}`}>
                {initials(who.name)}
              </div>
            )}
          </>
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
          {view === "services" && <Services key={openPackage ?? "board"} openPackage={openPackage} onWatchMailbox={() => setView("mail")} />}
          {view === "homeAutomation" && (
            <HomeAutomation
              onOpenPackage={(name) => {
                setOpenPackage(name);
                setView("services");
              }}
              onOpenServices={() => {
                setOpenPackage(undefined);
                setView("services");
              }}
              onNavigate={setView}
            />
          )}
          {view === "butler" && <Butler onEyebrow={setEyebrowOverride} onNavigate={setView} />}
          {view === "work" && <Work />}
          {view === "authority" && <ButlerAuthority />}
          {view === "memory" && <Memory />}
          {view === "integrations" && <Integrations />}
          {view === "mail" && <MailWatch onNavigate={setView} />}
          {view === "routines" && <Routines key={openRoutine ?? "list"} openRoutine={openRoutine} />}
          {view === "commitments" && <Commitments />}
          {view === "butlers" && <Butlers />}
          {view === "chat" && <Chat />}
          {view === "household" && <Household />}
          {view === "plans" && <Plans onNavigate={setView} />}
          {view === "activity" && (
            <Activity
              onOpenRoutine={(name) => {
                setOpenRoutine(name);
                setView("routines");
              }}
            />
          )}
          {view === "report" && <MorningReport onNavigate={setView} />}
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

// SignIn (ADR-W033 A4, A4.1): what the Console shows with no session.
//
// There is one way in: an active Owner signs in through the household's
// identity provider, as themselves, and comes back with a Console
// session. Usually nobody sees this screen — a 401 that a sign-in can fix
// sends the browser round it before anything renders. It shows when that
// could not happen: the page was reached without going through the
// household address, sign-in is not set up on this deployment, or a
// sign-in just came back refused.
//
// It never offers a token box. Bearer tokens are for `wild`, the
// installer and the host, and a pasted one could say neither who is
// signing in nor how recently.
export function SignIn() {
  return (
    <div className="gate">
      <div className="card">
        <div style={{ display: "flex", alignItems: "center", gap: "var(--space-3)", marginBottom: "var(--space-2)" }}>
          <ShieldCheck size={22} weight="fill" color="var(--color-accent)" />
          <h3 style={{ margin: 0 }}>Wildstone Console</h3>
        </div>
        <div className="muted" style={{ marginBottom: "var(--space-3)" }}>
          The Console is for the household's Owners. Sign in through your
          household's address as yourself, with the username and password you
          chose when Wildstone was installed.
        </div>
        <div className="muted" style={{ marginBottom: "var(--space-4)" }}>
          If sign-in is not set up on this deployment yet — it needs the
          identity service and the wildstone-console package — the Console
          cannot be opened. Until then, administer the house from the host
          with wild.
        </div>
        <button className="btn btn-primary btn-block" onClick={() => location.assign(signInURL())}>
          Sign in
        </button>
      </div>
    </div>
  );
}

// SignOut leaves the Console (ADR-W033 A4.1).
//
// It ends the Console session and then the household one at the identity
// provider: leaving only the Console would sign the person straight back
// in on their next visit.
function SignOut() {
  const [busy, setBusy] = useState(false);
  const leave = async () => {
    setBusy(true);
    const done = await api.consoleSignout().catch(() => ({}) as { signOut?: string });
    location.assign(done.signOut ?? "/");
  };
  return (
    <button className="btn btn-ghost" onClick={leave} disabled={busy}>
      Sign out
    </button>
  );
}


// StepUp is the Console's half of ADR-W033 A4.1's consequential gate.
//
// A drastic operation refused because the sign-in behind it is more than
// a few minutes old opens this: it says why, and sends the person to
// confirm their password with the identity provider. The action is
// remembered, and when they come back the screen that owns it reopens
// its final confirmation, so finishing is one click. Nothing is replayed
// for them: a write re-sent after a redirect is one they did not see
// themselves make.
//
// The note that they are back stays until they dismiss it. It used to be
// a toast gone in three seconds, which on a two-click action left
// somebody on the right page with no idea what to press.
function StepUp() {
  const [asked, setAsked] = useState<{ url: string; method: string; path: string } | null>(null);
  const [back, setBack] = useState(() => pendingStepUp() !== null);
  useEffect(() => {
    setOnStepUp((url, method, path) => setAsked({ url, method, path }));
  }, []);
  return (
    <>
      {asked && (
        <Confirm
          kind="warn"
          icon={<ShieldCheck size={18} weight="fill" />}
          title="Confirm your password"
          body={
            "This can't easily be undone, so the Console asks you to sign in again first. " +
            "You'll come back to this screen with it ready to finish."
          }
          confirmLabel="Confirm password"
          cancelLabel="Not now"
          onConfirm={() => {
            rememberStepUp(asked.method, asked.path);
            const here = location.pathname + location.search + location.hash;
            location.assign(`${asked.url}&return=${encodeURIComponent(here)}`);
          }}
          onCancel={() => setAsked(null)}
        />
      )}
      {back && (
        <div className="stepup-back" role="status">
          <ShieldCheck size={16} weight="fill" />
          <span>Password confirmed. Nothing was done while you were away: confirm it once more to finish.</span>
          <button
            className="btn btn-ghost"
            onClick={() => {
              clearStepUp();
              setBack(false);
            }}
          >
            Dismiss
          </button>
        </div>
      )}
    </>
  );
}
