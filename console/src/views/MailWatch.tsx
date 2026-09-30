import { useState } from "react";
import { EnvelopeSimple, Prohibit, Trash } from "@phosphor-icons/react";
import {
  ApiError,
  mailApi,
  type AgentSubscription,
  type MailActOutcome,
  type MailActResult,
  type MailCandidate,
  type MailCandidateAction,
  type MailCandidateEdit,
  type MailCandidateRow,
  type MailReview,
  type MailReviewKind,
  type MailMonitor,
  type MailSetup,
  type MailWatchChoice,
  type PlanRecord,
  MAIL_MATCH_KIND,
} from "../api";
import { ago, ErrorLine, useAction, useLoad, when } from "../lib";
import { useConsoleState } from "../state";
import { Confirm } from "../ui/Confirm";
import { SecretInput } from "../ui/SecretInput";
import { Kind, StatusPill } from "../ui/status";

// Watch this mailbox (epic #326 S07, #460).
//
// The screen a Person uses to let an Agent read their own selected mail
// for possible calendar events, and to see — and stop — what it does.
// Four things shape it.
//
// THE SERVER IS WHERE THE SETUP IS. Every step reads the setup read model
// (GET /api/v1/mail/setup) and decides from it which step this is. A
// refresh, a closed tab or a restarted Keystone lands on the same step,
// because nothing about the setup is kept in the browser — the form a
// subscription was filed with comes back from the pending Plan itself.
//
// TWO APPROVALS, NEVER ONE. An Owner approves the exact subscription; the
// mailbox's owner authorizes it. They are shown as two separate states,
// side by side, and neither is ever worded as if it covered the other.
//
// THE PASSWORD GOES ONE WAY. It is typed into the protected secret input,
// sent once, and cleared. Nothing here can show it again, because nothing
// the server sends back contains it.
//
// ONLY YOURS. The setup model carries the caller's own connections and
// nobody else's; the household list at the bottom is an Owner's, and is
// read from the endpoint that already decides who may see what.

// ---------------------------------------------------------------- pure logic

export type Stage =
  | "not-installed"
  | "no-connection"
  | "not-rendered"
  | "credential"
  | "choose"
  | "awaiting-owner"
  | "authorize"
  | "awaiting-render"
  | "watching";

export type OwnerApproval = "not-requested" | "waiting" | "approved";
export type PersonalAuthorization = "not-given" | "authorized";

export interface StageInfo {
  stage: Stage;
  owner: OwnerApproval;
  personal: PersonalAuthorization;
  // The subscription the monitor runs under, once an Owner approved one.
  sub?: AgentSubscription;
  // What the Person chose, from the subscription or the pending Plan.
  choice?: MailWatchChoice;
  // A subscription filed and not yet decided.
  pendingSubscription?: string;
  pendingRender?: string;
  // An earlier monitor was switched off; this is a fresh setup.
  wasDisabled: boolean;
}

// choiceFromMatch reads the choices a Watch-this-mailbox subscription
// was filed with back out of its match document.
export function choiceFromMatch(match: unknown): MailWatchChoice | undefined {
  let m = match;
  if (typeof m === "string") {
    try {
      m = JSON.parse(m);
    } catch {
      return undefined;
    }
  }
  if (!m || typeof m !== "object") return undefined;
  const o = m as Record<string, unknown>;
  const scope = (o.scope ?? {}) as Record<string, unknown>;
  const list = (v: unknown) => (Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : []);
  if (typeof o.agent !== "number" || typeof o.targetCalendar !== "string") return undefined;
  return {
    agent: o.agent,
    targetCalendar: o.targetCalendar,
    scope: { folders: list(scope.folders), senders: list(scope.senders), subjects: list(scope.subjects) },
    processingLocation: o.processingLocation === "cloud" ? "cloud" : "local",
    conflictCalendars: list(o.conflictCalendars),
  };
}

function matchDigest(sub: AgentSubscription): string | undefined {
  try {
    const m = JSON.parse(sub.match) as Record<string, unknown>;
    return typeof m.authorization === "string" ? m.authorization : undefined;
  } catch {
    return undefined;
  }
}

// currentSubscription is the monitor's live subscription: the newest
// enabled one for the connection.
export function currentSubscription(mon: MailMonitor): AgentSubscription | undefined {
  return mon.subscriptions
    .filter((s) => s.enabled && s.matchKind === MAIL_MATCH_KIND)
    .sort((a, b) => b.id - a.id)[0];
}

// isAuthorized: the Person's live authorization names this exact
// subscription AND the digest the subscription names. Anything less is
// an authorization for something else.
export function isAuthorized(mon: MailMonitor, sub?: AgentSubscription): boolean {
  const a = mon.authorization;
  return !!a && !!sub && !a.revokedAt && a.subscription === sub.id && matchDigest(sub) === a.digest;
}

export function stageOf(setup: MailSetup, mon?: MailMonitor): StageInfo {
  const base: StageInfo = { stage: "not-installed", owner: "not-requested", personal: "not-given", wasDisabled: false };
  if (!setup.provider) return base;
  if (!mon) return { ...base, stage: "no-connection" };
  if (!mon.connection.executable) return { ...base, stage: "not-rendered" };
  if (!mon.credential) return { ...base, stage: "credential" };
  const sub = currentSubscription(mon);
  const pendingSub = mon.pendingPlans.find((p) => p.kind === "subscription");
  const pendingRender = mon.pendingPlans.find((p) => p.kind === "render");
  const wasDisabled = !sub && mon.subscriptions.some((s) => !s.enabled);
  if (!sub) {
    if (pendingSub) {
      return {
        ...base,
        stage: "awaiting-owner",
        owner: "waiting",
        choice: choiceFromMatch(pendingSub.match),
        pendingSubscription: pendingSub.id,
        wasDisabled,
      };
    }
    return { ...base, stage: "choose", wasDisabled };
  }
  const owner: OwnerApproval = sub.awaitingApproval ? "waiting" : "approved";
  const personal: PersonalAuthorization = isAuthorized(mon, sub) ? "authorized" : "not-given";
  const info: StageInfo = {
    stage: "watching",
    owner,
    personal,
    sub,
    choice: choiceFromMatch(sub.match),
    pendingSubscription: pendingSub?.id,
    pendingRender: pendingRender?.id,
    wasDisabled: false,
  };
  if (personal === "not-given") return { ...info, stage: "authorize" };
  if (owner === "waiting") return { ...info, stage: "awaiting-owner" };
  if (pendingRender) return { ...info, stage: "awaiting-render" };
  return info;
}

// Go's time.Duration.String ("15m0s", "1h30m0s") in milliseconds.
export function durationMs(d?: string): number {
  if (!d) return 0;
  let total = 0;
  for (const [, n, unit] of d.matchAll(/([\d.]+)(h|ms|m|s)/g)) {
    const v = Number(n);
    total += unit === "h" ? v * 3_600_000 : unit === "m" ? v * 60_000 : unit === "s" ? v * 1000 : v;
  }
  return total;
}

export type Recovery = "reenter-password" | "resume" | "reauthorize" | "edit" | "contact-owner";

export type HealthState =
  | "waiting-first-run"
  | "ok"
  | "empty"
  | "paused"
  | "unavailable"
  | "stale"
  | "auth_failed"
  | "config_failed"
  | "erasure_unverified"
  | "degraded"
  | "not-authorized";

export interface Health {
  state: HealthState;
  kind: Kind;
  title: string;
  detail: string;
  recovery?: Recovery;
}

// healthOf says, in one state, what the monitor is doing and the next
// safe action. The order is the order of what must be fixed first: an
// unproven erasure before anything, a refused password before a pause,
// a missing authorization before a schedule that could not run anyway.
export function healthOf(mon: MailMonitor, sub: AgentSubscription, now: Date = new Date()): Health {
  const sched = sub.schedule;
  const conn = mon.connection;
  const last = sched?.lastOutcome;
  if (conn.degradedReason?.startsWith("erasure_unverified") || (mon.unverifiedErasures?.length ?? 0) > 0) {
    return {
      state: "erasure_unverified",
      kind: "bad",
      title: "An old password copy is not proven erased",
      detail:
        "A node has not proven it erased a password you replaced or removed. Nothing new is read with it, " +
        "but an Owner has to retry the erasure or update that node.",
      recovery: "contact-owner",
    };
  }
  if (sched?.hold === "auth_failed" || last === "auth_failed") {
    return {
      state: "auth_failed",
      kind: "bad",
      title: "Your mail provider refused the password",
      detail:
        "Watching stopped rather than retry, so your account is not locked out. Enter a new app password; " +
        "you will authorize the monitor again with it, then resume.",
      recovery: "reenter-password",
    };
  }
  if (sched?.hold === "config_failed" || last === "config_failed") {
    return {
      state: "config_failed",
      kind: "bad",
      title: "A folder or filter does not work with this mailbox",
      detail: sched?.holdReason || "Check the folder names and filters, then save them again.",
      recovery: "edit",
    };
  }
  if (!isAuthorized(mon, sub)) {
    return {
      state: "not-authorized",
      kind: "warn",
      title: "Not authorized by you",
      detail:
        "Nothing is read until you authorize this exact monitor. A change to the password, the settings or " +
        "the adapter needs your authorization again.",
      recovery: "reauthorize",
    };
  }
  if (sched?.hold === "paused") {
    return {
      state: "paused",
      kind: "mute",
      title: "Paused",
      detail: `Paused${sched.heldBy ? ` by ${sched.heldBy}` : ""}${sched.heldAt ? ` ${ago(sched.heldAt, now)}` : ""}. No mail is read until you resume.`,
      recovery: "resume",
    };
  }
  if (conn.state === "degraded") {
    return {
      state: "degraded",
      kind: "warn",
      title: "Running in a degraded state",
      detail: conn.degradedReason || "The adapter is not fully healthy.",
      recovery: "contact-owner",
    };
  }
  const failures = sched?.consecutiveFailures ?? 0;
  if (conn.state === "unavailable" || last === "unavailable" || last === "error" || last === "abandoned") {
    return {
      state: "unavailable",
      kind: failures >= 3 ? "bad" : "warn",
      title: "Your mail provider could not be reached",
      detail:
        failures >= 3
          ? `${failures} attempts in a row have failed. It keeps retrying; if it does not recover, an Owner can check the node it runs on.`
          : "It retries on its own with a growing delay. Nothing was skipped: the next run starts where the last good one stopped.",
      recovery: failures >= 3 ? "contact-owner" : undefined,
    };
  }
  const cadence = durationMs(sub.cadence) || 15 * 60_000;
  const committed = sub.checkpoint?.committedAt ? new Date(sub.checkpoint.committedAt).getTime() : 0;
  const staleCursor =
    !!sched?.lastRunAt && committed > 0 && now.getTime() - committed > Math.max(3 * cadence, 60 * 60_000);
  if (last === "stale" || staleCursor) {
    return {
      state: "stale",
      kind: "warn",
      title: "Behind",
      detail:
        last === "stale"
          ? "The last run saw only an old view of the mailbox, so nothing was committed from it. It retries on its own."
          : `The mailbox position has not moved since ${when(sub.checkpoint?.committedAt)}. If this persists, an Owner can check the node it runs on.`,
      recovery: last === "stale" ? undefined : "contact-owner",
    };
  }
  if (!sched?.lastRunAt) {
    return {
      state: "waiting-first-run",
      kind: "mute",
      title: "Waiting for the first run",
      detail: sched?.nextDueAt ? `First run due ${when(sched.nextDueAt)}.` : "It runs once the adapter is started.",
    };
  }
  if (last === "empty") {
    return { state: "empty", kind: "ok", title: "Watching — nothing new", detail: "The last run found no new mail in scope." };
  }
  return { state: "ok", kind: "ok", title: "Watching", detail: "The last run finished and its position is saved." };
}

// connectionNameFor is the Personal connection's name for a member:
// one mailbox per Person on this provider.
export function connectionNameFor(member: string): string {
  const slug = member.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
  return `${slug || "person"}-mail`;
}

export function isMailPlan(p: PlanRecord): boolean {
  return (
    p.summary.startsWith("adapter placement for integration/") ||
    p.summary.includes("watches the mailbox") ||
    p.summary.includes("edits the mailbox monitor")
  );
}

const list = (s: string) =>
  s
    .split(/[\n,]/)
    .map((v) => v.trim())
    .filter(Boolean);

// ---------------------------------------------------------------- loader

export type Navigate = (to: "plans" | "services") => void;

export function MailWatch({ onNavigate }: { onNavigate: Navigate }) {
  const setup = useLoad(() => mailApi.setup(), []);
  const { snap } = useConsoleState();
  const candidates = useLoad(() => mailApi.candidates(), []);
  const household = useLoad(
    () => (setup.data?.owner ? mailApi.subscriptions() : Promise.resolve({ subscriptions: [] })),
    [setup.data?.owner],
  );
  if (setup.error) return <ErrorLine error={setup.error} />;
  if (!setup.data) return <div className="muted">Reading your mailbox setup…</div>;
  const reviewUnavailable =
    candidates.error !== null && /not found|404/i.test(candidates.error) ? true : undefined;
  return (
    <MailWatchView
      setup={setup.data}
      candidates={candidates.data?.candidates ?? []}
      candidatesError={reviewUnavailable ? null : candidates.error}
      reviewUnavailable={reviewUnavailable}
      household={(household.data?.subscriptions ?? []).filter((s) => s.matchKind === MAIL_MATCH_KIND)}
      pendingMailPlans={(snap?.pending ?? []).filter(isMailPlan)}
      nodes={(snap?.nodes ?? []).filter((n) => n.connected).map((n) => n.id)}
      onChanged={() => {
        setup.reload();
        candidates.reload();
        household.reload();
      }}
      onNavigate={onNavigate}
    />
  );
}

// ---------------------------------------------------------------- entry points

// MailWatchEntry is how the installed email capability, and Home, lead
// here: a line saying what it is, and the way in.
export function MailWatchEntry({ onOpen }: { onOpen?: () => void }) {
  return (
    <>
      <div className="sect" style={{ marginTop: "var(--space-6)" }}>
        <span className="eyebrow">Watch this mailbox</span>
        <span className="fade" />
      </div>
      <div className="muted">
        Each person can let an Agent look at mail they choose for possible calendar events. It reads only,
        asks before adding anything, and stays off until that person turns it on for their own mailbox.
      </div>
      {onOpen && (
        <div className="row">
          <button className="btn btn-secondary" onClick={onOpen}>
            <EnvelopeSimple size={14} /> Watch this mailbox
          </button>
        </div>
      )}
    </>
  );
}

// ---------------------------------------------------------------- the screen

export function MailWatchView({
  setup,
  candidates,
  candidatesError = null,
  reviewUnavailable,
  household = [],
  pendingMailPlans = [],
  nodes = [],
  onChanged,
  onNavigate,
  now,
}: {
  setup: MailSetup;
  candidates: MailCandidateRow[];
  candidatesError?: string | null;
  reviewUnavailable?: boolean;
  household?: AgentSubscription[];
  pendingMailPlans?: PlanRecord[];
  nodes?: string[];
  onChanged: () => void;
  onNavigate: Navigate;
  now?: Date;
}) {
  const mon = setup.monitors[0];
  const info = stageOf(setup, mon);
  return (
    <>
      <div className="sub">
        Let an Agent look at new mail you choose for possible calendar events, and ask you before adding
        anything. It only reads: it cannot send, delete, move or mark mail, and your webmail and calendars
        work exactly as before.
      </div>

      <ProviderSettings setup={setup} onNavigate={onNavigate} />

      {info.stage !== "not-installed" && (
        <section className="work-section">
          <div className="sect">
            <span className="eyebrow">Watch this mailbox</span>
            <span className="fade" />
          </div>
          <SetupStep setup={setup} mon={mon} info={info} nodes={nodes} onChanged={onChanged} onNavigate={onNavigate} />
        </section>
      )}

      {mon && info.sub && (
        <HealthPanel setup={setup} mon={mon} info={info} sub={info.sub} onChanged={onChanged} now={now} />
      )}

      {mon && (
        <CandidateList
          candidates={candidates}
          error={candidatesError}
          unavailable={reviewUnavailable}
          onChanged={onChanged}
          now={now}
        />
      )}

      {setup.owner && (
        <HouseholdMonitors subs={household} pending={pendingMailPlans} onNavigate={onNavigate} />
      )}
    </>
  );
}

// The provider this installation already configured. Read-only: V1 has
// one provider per install, and the adapter inherits it.
const SETTING_LABELS: Record<string, string> = {
  imap_host: "IMAP server",
  imap_port: "Port",
  imap_encryption: "Encryption",
  username_domain: "Username domain",
};

export function ProviderSettings({ setup, onNavigate }: { setup: MailSetup; onNavigate: Navigate }) {
  if (!setup.provider) {
    return (
      <div className="callout">
        Email is not installed, so there is no mailbox to watch. Install the mail capability first.{" "}
        <button className="btn btn-ghost" onClick={() => onNavigate("services")}>
          Open Services
        </button>
      </div>
    );
  }
  const s = setup.provider.settings;
  return (
    <section className="work-section">
      <div className="sect">
        <span className="eyebrow">Your mail provider</span>
        <span className="muted">from {setup.provider.package}</span>
        <span className="fade" />
      </div>
      <dl className="work-evidence" aria-label="mail provider settings (read-only)">
        {Object.keys(SETTING_LABELS).map((k) => (
          <div key={k}>
            <dt className="muted">{SETTING_LABELS[k]}</dt>
            <dd>{s[k] ? <span className="tnum">{s[k]}</span> : <span className="muted">none</span>}</dd>
          </div>
        ))}
      </dl>
      <div className="muted">
        Set when email was installed, and shared by everyone in the household. Each person watches their
        own mailbox with their own password.
      </div>
    </section>
  );
}

// The two approvals, side by side and never merged.
export function ApprovalStates({ owner, personal }: { owner: OwnerApproval; personal: PersonalAuthorization }) {
  return (
    <div className="row" aria-label="approval states">
      <StatusPill kind={owner === "approved" ? "ok" : owner === "waiting" ? "warn" : "mute"}>
        {owner === "approved"
          ? "Approved by an Owner"
          : owner === "waiting"
            ? "Waiting for Owner approval"
            : "Owner approval not requested yet"}
      </StatusPill>
      <StatusPill kind={personal === "authorized" ? "ok" : "mute"}>
        {personal === "authorized" ? "Authorized by you" : "Not yet authorized by you"}
      </StatusPill>
    </div>
  );
}

function SetupStep({
  setup,
  mon,
  info,
  nodes,
  onChanged,
  onNavigate,
}: {
  setup: MailSetup;
  mon?: MailMonitor;
  info: StageInfo;
  nodes: string[];
  onChanged: () => void;
  onNavigate: Navigate;
}) {
  switch (info.stage) {
    case "no-connection":
      return <PrepareStep setup={setup} nodes={nodes} onChanged={onChanged} />;
    case "not-rendered":
      return <RenderStep setup={setup} mon={mon!} onChanged={onChanged} />;
    case "credential":
      return (
        <>
          <Steps at={1} />
          <CredentialForm setup={setup} mon={mon!} onChanged={onChanged} />
        </>
      );
    case "choose":
      return (
        <>
          <Steps at={2} />
          {info.wasDisabled && (
            <div className="callout">Your previous monitor was switched off. Choose its settings again to restart it.</div>
          )}
          <WatchForm setup={setup} mon={mon!} onChanged={onChanged} />
        </>
      );
    case "awaiting-owner":
      return (
        <>
          <Steps at={3} />
          <ApprovalStates owner={info.owner} personal={info.personal} />
          <div className="callout">
            An Owner reviews this exact monitor — which mail, which Agent, which calendars — in Plans. Their
            approval is not your authorization: once they approve, you authorize it here.
          </div>
          {info.choice && <ChoiceSummary setup={setup} choice={info.choice} />}
          {setup.owner && (
            <button className="btn btn-secondary" onClick={() => onNavigate("plans")}>
              Review in Plans
            </button>
          )}
          {!info.sub && <WithdrawFiling mon={mon!} setup={setup} info={info} onChanged={onChanged} />}
        </>
      );
    case "authorize":
      return (
        <>
          <Steps at={4} />
          <ApprovalStates owner={info.owner} personal={info.personal} />
          <AuthorizeStep setup={setup} mon={mon!} info={info} onChanged={onChanged} />
        </>
      );
    case "awaiting-render":
      return (
        <>
          <Steps at={5} />
          <ApprovalStates owner={info.owner} personal={info.personal} />
          <div className="callout">
            Both approvals are in. An Owner now approves starting the mail adapter for your connection; it
            begins reading once that Plan is approved.
          </div>
          {setup.owner && (
            <button className="btn btn-secondary" onClick={() => onNavigate("plans")}>
              Review in Plans
            </button>
          )}
        </>
      );
    case "watching":
      return <ApprovalStates owner={info.owner} personal={info.personal} />;
    default:
      return null;
  }
}

const STEP_NAMES = ["Password", "What to watch", "Owner approval", "Your authorization", "Start"];

function Steps({ at }: { at: number }) {
  return (
    <ol className="chips" aria-label="setup steps">
      {STEP_NAMES.map((name, i) => (
        <li key={name} className="chip" aria-current={i + 1 === at ? "step" : undefined}>
          <StatusPill kind={i + 1 < at ? "ok" : i + 1 === at ? "warn" : "mute"}>
            {i + 1}. {name}
          </StatusPill>
        </li>
      ))}
    </ol>
  );
}

// An Owner prepares the connection: it exists, it has a node to run on,
// and its adapter is recorded with node-bound tokens. Nothing reads mail.
function PrepareStep({ setup, nodes, onChanged }: { setup: MailSetup; nodes: string[]; onChanged: () => void }) {
  const [picked, setNode] = useState("");
  // The node list arrives with the shell's poll, possibly after this
  // mounted; until somebody picks, the first connected node is the pick.
  const node = picked || nodes[0] || "";
  const act = useAction();
  const name = connectionNameFor(setup.member);
  if (!setup.owner) {
    return (
      <div className="callout">
        An Owner prepares a mailbox connection for you first. It reads nothing; it only makes a place for
        your password.
      </div>
    );
  }
  return (
    <>
      <div className="note">
        First, a connection for your mailbox ({name}) and a node to run its reader on. This reads no mail
        and asks for no password yet.
      </div>
      <div className="field">
        <label htmlFor="mail-node">Run it on</label>
        <select id="mail-node" value={node} onChange={(e) => setNode(e.target.value)}>
          {nodes.length === 0 && <option value="">no connected node</option>}
          {nodes.map((n) => (
            <option key={n} value={n}>
              {n}
            </option>
          ))}
        </select>
      </div>
      <button
        className="btn btn-primary"
        disabled={!node || act.state.phase === "busy"}
        onClick={() =>
          act.run(async () => {
            await mailApi.createConnection(name);
            await mailApi.addPlacement(name, node, "primary", 0);
            await mailApi.renderAdapter(name);
            onChanged();
          }, "Prepared.")
        }
      >
        Prepare my mailbox connection
      </button>
      {act.state.phase === "error" && <ErrorLine error={act.state.message} />}
    </>
  );
}

function RenderStep({ setup, mon, onChanged }: { setup: MailSetup; mon: MailMonitor; onChanged: () => void }) {
  const act = useAction();
  if (!setup.owner) {
    return <div className="callout">Waiting for an Owner to prepare the mail adapter for {mon.connection.name}.</div>;
  }
  return (
    <>
      <div className="note">
        The mail adapter has not been prepared for {mon.connection.name} yet. Preparing records which adapter
        version it runs and gives each node its own token; it reads no mail.
      </div>
      <button
        className="btn btn-primary"
        disabled={act.state.phase === "busy"}
        onClick={() => act.run(async () => (await mailApi.renderAdapter(mon.connection.name), onChanged()), "Prepared.")}
      >
        Prepare the adapter
      </button>
      {act.state.phase === "error" && <ErrorLine error={act.state.message} />}
    </>
  );
}

// The protected intake. The password field is the shared secret input:
// hidden by default, never spell-checked, emptied after every submit —
// success or not — so what was typed is on the page for as short a time
// as possible and is never shown back.
export function CredentialForm({
  setup,
  mon,
  replacing = false,
  onChanged,
}: {
  setup: MailSetup;
  mon: MailMonitor;
  replacing?: boolean;
  onChanged: () => void;
}) {
  const domain = setup.provider?.settings.username_domain;
  const [login, setLogin] = useState(mon.credential?.login ?? "");
  const [password, setPassword] = useState("");
  const act = useAction();
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        const typed = password;
        setPassword("");
        void act.run(async () => {
          await mailApi.submitCredential(mon.connection.name, login.trim(), typed);
          onChanged();
        }, replacing ? "Replaced. Authorize the monitor again to use the new password." : "Stored on the node that reads your mail.");
      }}
    >
      <div className="callout">
        Use an <strong>app password</strong> made just for this — from your mail provider's security
        settings — not your account password. It is easy to revoke on its own, and Gmail, Outlook and
        Yahoo refuse an account password here anyway.
      </div>
      <div className="field">
        <label htmlFor="mail-login">
          Mailbox username
          {domain && <span className="muted">A bare name gets @{domain} added, as in webmail.</span>}
        </label>
        <input
          id="mail-login"
          autoComplete="username"
          spellCheck={false}
          value={login}
          onChange={(e) => setLogin(e.target.value)}
        />
      </div>
      <div className="field">
        <label htmlFor="mail-app-password">
          App password
          <span className="muted">
            Sent once to the node that reads your mail. Wildstone does not keep it and cannot show it again.
          </span>
        </label>
        <SecretInput
          id="mail-app-password"
          aria-label="mailbox app password"
          autoComplete="new-password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
        />
      </div>
      <button className="btn btn-primary" type="submit" disabled={!login.trim() || !password || act.state.phase === "busy"}>
        {replacing ? "Replace password" : "Save password"}
      </button>
      {act.state.phase === "done" && <div className="msg-note">{act.state.message}</div>}
      {act.state.phase === "error" && <ErrorLine error={act.state.message} />}
    </form>
  );
}

const CADENCES = ["5m0s", "15m0s", "30m0s", "1h0m0s", "6h0m0s", "24h0m0s"];

function cadenceLabel(d: string): string {
  const ms = durationMs(d);
  if (ms >= 3_600_000) return `every ${ms / 3_600_000}h`;
  return `every ${ms / 60_000} min`;
}

export function WatchForm({
  setup,
  mon,
  initial,
  editing,
  onChanged,
  onCancel,
}: {
  setup: MailSetup;
  mon: MailMonitor;
  initial?: MailWatchChoice;
  editing?: boolean;
  onChanged: () => void;
  onCancel?: () => void;
}) {
  const writable = setup.calendars.filter((c) => c.writable);
  const [folders, setFolders] = useState((initial?.scope.folders ?? ["INBOX"]).join("\n"));
  const [senders, setSenders] = useState((initial?.scope.senders ?? []).join("\n"));
  const [subjects, setSubjects] = useState((initial?.scope.subjects ?? []).join("\n"));
  const [cadence, setCadence] = useState(setup.cadence.default);
  const [location, setLocation] = useState<"local" | "cloud">(initial?.processingLocation ?? "local");
  const [agent, setAgent] = useState<number>(initial?.agent ?? setup.agents[0]?.seq ?? 0);
  const [target, setTarget] = useState(initial?.targetCalendar ?? writable[0]?.id ?? "");
  const [conflicts, setConflicts] = useState<string[]>(
    initial?.conflictCalendars ?? (writable[0] ? [writable[0].id] : []),
  );
  const act = useAction();
  const cloudAllowed = mon.connection.cloudAllowed;
  const ready = agent > 0 && target !== "" && list(folders).length > 0;

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        void act.run(async () => {
          await mailApi.fileSubscription(mon.connection.name, {
            agent,
            targetCalendar: target,
            scope: { folders: list(folders), senders: list(senders), subjects: list(subjects) },
            processingLocation: location,
            cadence: editing ? undefined : cadence,
            conflictCalendars: conflicts,
          });
          onChanged();
        }, editing ? "Saved. Waiting for an Owner to approve the change." : "Sent for an Owner's approval.");
      }}
    >
      {editing && (
        <div className="callout callout-warn">
          Saving a change stops this monitor at once. It starts again only after an Owner approves the new
          settings and you authorize them.
        </div>
      )}
      <div className="field">
        <label htmlFor="mail-folders">
          Folders
          <span className="muted">One per line. INBOX is the usual one.</span>
        </label>
        <textarea id="mail-folders" rows={2} value={folders} onChange={(e) => setFolders(e.target.value)} />
      </div>
      <div className="field">
        <label htmlFor="mail-senders">
          Only from these senders or domains <span className="muted">Optional, one per line — e.g. school.example.org</span>
        </label>
        <textarea id="mail-senders" rows={2} value={senders} onChange={(e) => setSenders(e.target.value)} />
      </div>
      <div className="field">
        <label htmlFor="mail-subjects">
          Only with a subject containing <span className="muted">Optional, one per line</span>
        </label>
        <textarea id="mail-subjects" rows={2} value={subjects} onChange={(e) => setSubjects(e.target.value)} />
      </div>
      {!editing && (
        <div className="field">
          <label htmlFor="mail-cadence">How often to check</label>
          <select id="mail-cadence" value={cadence} onChange={(e) => setCadence(e.target.value)}>
            {CADENCES.map((c) => (
              <option key={c} value={c}>
                {cadenceLabel(c)}
              </option>
            ))}
          </select>
        </div>
      )}
      <fieldset className="field">
        <legend>Where your mail is read</legend>
        <label>
          <input type="radio" name="mail-location" checked={location === "local"} onChange={() => setLocation("local")} />{" "}
          On this household's own machines (recommended)
        </label>
        <label>
          <input
            type="radio"
            name="mail-location"
            checked={location === "cloud"}
            disabled={!cloudAllowed}
            onChange={() => setLocation("cloud")}
          />{" "}
          A cloud model may read it
        </label>
        <div className="muted">
          Cloud stays off unless you choose it{cloudAllowed ? "" : ", and an Owner has not allowed it for this connection"}.
          Without a local model, only calendar invitations are read and other mail is marked as needing
          details — it never falls back to the cloud.
        </div>
      </fieldset>
      <div className="field">
        <label htmlFor="mail-agent">Agent</label>
        <select id="mail-agent" value={agent} onChange={(e) => setAgent(Number(e.target.value))}>
          {setup.agents.length === 0 && <option value={0}>no Agent you may use</option>}
          {setup.agents.map((a) => (
            <option key={a.seq} value={a.seq}>
              {a.name}
            </option>
          ))}
        </select>
      </div>
      <div className="field">
        <label htmlFor="mail-target">
          Add events to <span className="muted">Only calendars you may add to are listed.</span>
        </label>
        <select id="mail-target" value={target} onChange={(e) => setTarget(e.target.value)}>
          {writable.length === 0 && <option value="">no calendar you may add to</option>}
          {writable.map((c) => (
            <option key={c.id} value={c.id}>
              {c.name}
            </option>
          ))}
        </select>
      </div>
      <fieldset className="field">
        <legend>Check these calendars for conflicts</legend>
        {setup.calendars.map((c) => (
          <label key={c.id}>
            <input
              type="checkbox"
              checked={conflicts.includes(c.id)}
              onChange={(e) =>
                setConflicts((cur) => (e.target.checked ? [...cur, c.id] : cur.filter((x) => x !== c.id)))
              }
            />{" "}
            {c.name}
          </label>
        ))}
        {setup.calendarsError && <ErrorLine error={`Your calendars could not be read: ${setup.calendarsError}`} />}
        <div className="muted">Only calendars you can see are offered, and a conflict names only what you could see anyway.</div>
      </fieldset>
      <div className="row">
        <button className="btn btn-primary" type="submit" disabled={!ready || act.state.phase === "busy"}>
          {editing ? "Save and ask for approval" : "Ask an Owner to approve"}
        </button>
        {onCancel && (
          <button className="btn btn-ghost" type="button" onClick={onCancel}>
            Keep the current settings
          </button>
        )}
      </div>
      {act.state.phase === "error" && <ErrorLine error={act.state.message} />}
    </form>
  );
}

function calendarName(setup: MailSetup, id: string) {
  return setup.calendars.find((c) => c.id === id)?.name ?? id;
}

export function ChoiceSummary({ setup, choice }: { setup: MailSetup; choice: MailWatchChoice }) {
  const agent = setup.agents.find((a) => a.seq === choice.agent)?.name ?? `Agent ${choice.agent}`;
  return (
    <ul className="work-evidence" aria-label="what this monitor watches">
      <li>Folders: {choice.scope.folders.join(", ") || "INBOX"}</li>
      {!!choice.scope.senders?.length && <li>From: {choice.scope.senders.join(", ")}</li>}
      {!!choice.scope.subjects?.length && <li>Subject contains: {choice.scope.subjects.join(", ")}</li>}
      <li>Read {choice.processingLocation === "cloud" ? "by a cloud model" : "on the household's own machines"}</li>
      <li>Agent: {agent}</li>
      <li>Adds to: {calendarName(setup, choice.targetCalendar)}</li>
      {!!choice.conflictCalendars?.length && (
        <li>Checks for conflicts: {choice.conflictCalendars.map((c) => calendarName(setup, c)).join(", ")}</li>
      )}
    </ul>
  );
}

// The Person's own act: authorize the subscription an Owner approved,
// with exactly the choices it names.
function AuthorizeStep({
  setup,
  mon,
  info,
  onChanged,
}: {
  setup: MailSetup;
  mon: MailMonitor;
  info: StageInfo;
  onChanged: () => void;
}) {
  const act = useAction();
  if (!info.sub || !info.choice) {
    return <ErrorLine error="This monitor's settings could not be read back; set them again." />;
  }
  const sub = info.sub;
  const choice = info.choice;
  return (
    <>
      <div className="note">
        {info.owner === "approved"
          ? "An Owner approved this monitor. It reads nothing until you authorize it yourself."
          : "Your change is waiting for an Owner. You can authorize it now; it runs once both are done."}
      </div>
      <ChoiceSummary setup={setup} choice={choice} />
      <button
        className="btn btn-primary"
        disabled={act.state.phase === "busy"}
        onClick={() =>
          act.run(async () => {
            await mailApi.authorize(mon.connection.name, choice, sub.id);
            onChanged();
          }, "Authorized by you.")
        }
      >
        Authorize this monitor
      </button>
      {act.state.phase === "error" && <ErrorLine error={act.state.message} />}
    </>
  );
}

// Changing your mind before an Owner decided: file again with other
// settings (which supersedes the first filing).
function WithdrawFiling({
  setup,
  mon,
  info,
  onChanged,
}: {
  setup: MailSetup;
  mon: MailMonitor;
  info: StageInfo;
  onChanged: () => void;
}) {
  const [editing, setEditing] = useState(false);
  if (!editing) {
    return (
      <button className="btn btn-ghost" onClick={() => setEditing(true)}>
        Change these settings
      </button>
    );
  }
  return (
    <WatchForm
      setup={setup}
      mon={mon}
      initial={info.choice}
      onChanged={() => {
        setEditing(false);
        onChanged();
      }}
      onCancel={() => setEditing(false)}
    />
  );
}

// ---------------------------------------------------------------- health

const OUTCOME_LABEL: Record<string, string> = {
  ok: "found mail in scope",
  empty: "nothing new",
  unavailable: "provider unreachable",
  stale: "only an old view",
  auth_failed: "password refused",
  config_failed: "folder or filter problem",
  error: "Wildstone error",
  abandoned: "did not finish",
};

export function HealthPanel({
  setup,
  mon,
  info,
  sub,
  onChanged,
  now = new Date(),
}: {
  setup: MailSetup;
  mon: MailMonitor;
  info: StageInfo;
  sub: AgentSubscription;
  onChanged: () => void;
  now?: Date;
}) {
  const health = healthOf(mon, sub, now);
  const sched = sub.schedule;
  const act = useAction();
  const [reentering, setReentering] = useState(false);
  const [editing, setEditing] = useState(false);
  const [confirm, setConfirm] = useState<"disable" | "revoke" | null>(null);
  const [cadence, setCadence] = useState(sub.cadence ?? setup.cadence.default);
  const paused = sched?.hold === "paused" || sched?.hold === "auth_failed" || sched?.hold === "config_failed";
  const name = mon.connection.name;
  const run = (fn: () => Promise<unknown>, done: string) => act.run(async () => (await fn(), onChanged()), done);

  return (
    <section className="work-section" aria-label="mailbox monitor health">
      <div className="sect">
        <span className="eyebrow">Health</span>
        <StatusPill kind={health.kind}>{health.title}</StatusPill>
        <span className="fade" />
      </div>
      <div className={health.kind === "bad" ? "callout callout-bad" : health.kind === "warn" ? "callout callout-warn" : "note"}>
        {health.detail}
        {health.recovery && (
          <div className="row">
            {health.recovery === "reenter-password" && (
              <button className="btn btn-primary" onClick={() => setReentering(true)}>
                Enter a new app password
              </button>
            )}
            {health.recovery === "resume" && (
              <button className="btn btn-primary" onClick={() => run(() => mailApi.controlSubscription(sub.id, "resume"), "Resumed.")}>
                Resume
              </button>
            )}
            {health.recovery === "reauthorize" && info.choice && (
              <button
                className="btn btn-primary"
                onClick={() => run(() => mailApi.authorize(name, info.choice!, sub.id), "Authorized by you.")}
              >
                Authorize again
              </button>
            )}
            {health.recovery === "edit" && (
              <button className="btn btn-primary" onClick={() => setEditing(true)}>
                Edit folders and filters
              </button>
            )}
            {health.recovery === "contact-owner" &&
              (setup.owner && health.state === "erasure_unverified" ? (
                <button className="btn btn-primary" onClick={() => run(() => mailApi.eraseCredential(name), "Erasure retried.")}>
                  Retry the erasure
                </button>
              ) : (
                <span className="muted">Ask an Owner to look at it{mon.nodes.length ? ` (it runs on ${mon.nodes.join(", ")})` : ""}.</span>
              ))}
          </div>
        )}
      </div>

      <dl className="work-evidence">
        <div>
          <dt className="muted">Last run</dt>
          <dd>
            {sched?.lastRunAt ? `${ago(sched.lastRunAt, now)} — ${OUTCOME_LABEL[sched.lastOutcome ?? ""] ?? sched.lastOutcome ?? "running"}` : "not yet"}
          </dd>
        </div>
        <div>
          <dt className="muted">Last success</dt>
          <dd>{sched?.lastSuccessAt ? ago(sched.lastSuccessAt, now) : "not yet"}</dd>
        </div>
        <div>
          <dt className="muted">Mailbox position saved</dt>
          <dd>
            {sub.checkpoint?.committedAt
              ? `${ago(sub.checkpoint.committedAt, now)} (checkpoint ${sub.checkpoint.version})`
              : "not yet — it starts from new mail"}
          </dd>
        </div>
        <div>
          <dt className="muted">Next check</dt>
          <dd>{paused ? "held" : sched?.nextDueAt ? when(sched.nextDueAt) : "—"}</dd>
        </div>
        {(sched?.consecutiveFailures ?? 0) > 0 && (
          <div>
            <dt className="muted">Failures in a row</dt>
            <dd className="tnum">{sched!.consecutiveFailures}</dd>
          </div>
        )}
        {sched?.lastError && (
          <div>
            <dt className="muted">Last error</dt>
            <dd>{sched.lastError}</dd>
          </div>
        )}
      </dl>

      {reentering && (
        <CredentialForm
          setup={setup}
          mon={mon}
          replacing
          onChanged={() => {
            setReentering(false);
            onChanged();
          }}
        />
      )}
      {editing && (
        <WatchForm
          setup={setup}
          mon={mon}
          initial={info.choice}
          editing
          onChanged={() => {
            setEditing(false);
            onChanged();
          }}
          onCancel={() => setEditing(false)}
        />
      )}

      <div className="row" aria-label="monitor controls">
        {paused ? (
          <button className="btn btn-secondary" onClick={() => run(() => mailApi.controlSubscription(sub.id, "resume"), "Resumed.")}>
            Resume
          </button>
        ) : (
          <button className="btn btn-secondary" onClick={() => run(() => mailApi.controlSubscription(sub.id, "pause", { reason: "paused by its Person" }), "Paused.")}>
            Pause
          </button>
        )}
        <label>
          <span className="muted">Check</span>{" "}
          <select aria-label="how often to check" value={cadence} onChange={(e) => setCadence(e.target.value)}>
            {[...new Set([...CADENCES, sub.cadence ?? setup.cadence.default])].map((c) => (
              <option key={c} value={c}>
                {cadenceLabel(c)}
              </option>
            ))}
          </select>
        </label>
        {cadence !== sub.cadence && (
          <button className="btn btn-ghost" onClick={() => run(() => mailApi.controlSubscription(sub.id, "cadence", { cadence }), "Saved.")}>
            Save how often
          </button>
        )}
        {!editing && (
          <button className="btn btn-ghost" onClick={() => setEditing(true)}>
            Edit settings…
          </button>
        )}
        {!reentering && health.recovery !== "reenter-password" && (
          <button className="btn btn-ghost" onClick={() => setReentering(true)}>
            Replace password…
          </button>
        )}
        <button className="btn btn-ghost" onClick={() => setConfirm("disable")}>
          Stop watching…
        </button>
        <button className="btn btn-ghost" onClick={() => setConfirm("revoke")}>
          Remove password…
        </button>
      </div>
      {act.state.phase === "done" && <div className="msg-note">{act.state.message}</div>}
      {act.state.phase === "error" && <ErrorLine error={act.state.message} />}

      {confirm === "disable" && (
        <Confirm
          kind="warn"
          icon={<Prohibit size={18} />}
          title="Stop watching this mailbox?"
          body="No new mail is read from now on. Your webmail, your calendars, your mail at the provider and anyone else's monitor are not touched. Starting again means setting it up again, with an Owner's approval."
          confirmLabel="Stop watching"
          cancelLabel="Keep watching"
          busy={act.state.phase === "busy"}
          onConfirm={() => {
            setConfirm(null);
            void run(() => mailApi.controlSubscription(sub.id, "disable", { reason: "stopped by its Person" }), "Stopped.");
          }}
          onCancel={() => setConfirm(null)}
        />
      )}
      {confirm === "revoke" && (
        <Confirm
          kind="bad"
          icon={<Trash size={18} />}
          title="Remove your app password?"
          body="Access ends first, then each node that held it proves it erased. Revoke the app password at your mail provider too: only the provider can make it stop working everywhere. Webmail and calendars are not affected."
          confirmLabel="Remove password"
          cancelLabel="Keep it"
          busy={act.state.phase === "busy"}
          onConfirm={() => {
            setConfirm(null);
            void run(() => mailApi.revokeCredential(name), "Removed. Revoke it at your provider as well.");
          }}
          onCancel={() => setConfirm(null)}
        />
      )}
    </section>
  );
}

// ---------------------------------------------------------------- candidates

// The review is S06's (ADR-W030 A5.7): every row is the caller's own,
// every offer names one revision, and an action is sent with the token
// the candidate's GET returned for exactly that action. Nothing here
// decides what may be done — the review's own `actions` do — and a 409
// is always "nothing changed; look again".

const FIELD_LABEL: Record<string, string> = {
  title: "title",
  start: "start time",
  end: "end time",
  date: "date",
  time: "time",
  timezone: "time zone",
  tz: "time zone",
  location: "place",
  duration: "length",
  recurrence: "repeat",
};

function fieldList(names: string[] | null | undefined): string {
  return (names ?? []).map((n) => FIELD_LABEL[n] ?? n).join(", ");
}

// needsDetails mirrors the server's rule: anything but the place missing
// or ambiguous holds a candidate back from Add.
export function needsDetails(c: MailCandidate): boolean {
  if (c.state === "needs_details") return true;
  return [...(c.fields.missing ?? []), ...(c.fields.ambiguous ?? [])].some((n) => n !== "location");
}

// openReview is the row's review while it can still be answered.
export function openReview(row: MailCandidateRow, now: Date = new Date()): MailReview | undefined {
  const rv = row.review;
  if (!rv || rv.state !== "open" || rv.revision !== row.revision) return undefined;
  if (rv.expiresAt && new Date(rv.expiresAt).getTime() <= now.getTime()) return undefined;
  return rv;
}

// awaitsYou: an unresolved candidate, or an event already added whose
// correction or cancellation is waiting on an open review.
export function awaitsYou(row: MailCandidateRow, now: Date = new Date()): boolean {
  return row.state === "proposed" || row.state === "needs_details" || openReview(row, now) !== undefined;
}

// actionsFor is what the Person may do with this row: the open review's
// own actions, or — with no review open yet — the unresolved candidate's
// usual Edit/Dismiss (Add only once nothing is missing).
export function actionsFor(row: MailCandidateRow, now: Date = new Date()): MailCandidateAction[] {
  const rv = openReview(row, now);
  if (rv) return rv.actions ?? [];
  if (row.state === "proposed") return ["add", "edit", "dismiss"];
  if (row.state === "needs_details") return ["edit", "dismiss"];
  return [];
}

export const MAIL_OUTCOME_NOTE: Record<MailActOutcome, string> = {
  added: "Added to your calendar.",
  updated: "The event on your calendar was changed.",
  cancelled: "The event was removed from your calendar.",
  dismissed: "Dismissed. You will not be asked about this message again.",
  kept: "The event stays on your calendar.",
  revised: "Saved as a new version. Look it over and decide again.",
  returned_to_review: "Nothing was changed: something moved since it was offered. Look at it again.",
  pending_reconciliation: "Your calendar did not answer in time. It is being checked; nothing will be added twice.",
};

function eventWhen(f: MailCandidate["fields"]): string {
  if (!f.start) return "time not known";
  const start = new Date(f.start);
  if (f.allDay) return `${start.toLocaleDateString()} (all day)`;
  const end = f.end ? new Date(f.end) : f.durationMinutes ? new Date(start.getTime() + f.durationMinutes * 60_000) : null;
  return `${start.toLocaleString()}${end ? ` – ${end.toLocaleTimeString()}` : ""}${f.tz ? ` (${f.tz})` : ""}`;
}

export function CandidateList({
  candidates,
  error,
  unavailable,
  onChanged,
  now,
}: {
  candidates: MailCandidateRow[];
  error: string | null;
  unavailable?: boolean;
  onChanged: () => void;
  now?: Date;
}) {
  const open = candidates.filter((c) => awaitsYou(c, now));
  return (
    <section className="work-section" aria-label="events found in your mail">
      <div className="sect">
        <span className="eyebrow">Events found in your mail</span>
        <span className="muted tnum">{open.length}</span>
        <span className="fade" />
      </div>
      {unavailable ? (
        <div className="muted">Reviewing found events is not available on this Keystone yet.</div>
      ) : error ? (
        <ErrorLine error={error} />
      ) : open.length === 0 ? (
        <div className="muted">Nothing waiting. Nothing is ever added to a calendar without you.</div>
      ) : (
        open.map((c) => (
          <CandidateCard key={`${c.id}@${c.revision}`} candidate={c} onChanged={onChanged} now={now} />
        ))
      )}
    </section>
  );
}

function toLocalInput(iso?: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function fromLocalInput(v: string): string | undefined {
  if (!v) return undefined;
  const d = new Date(v);
  return isNaN(d.getTime()) ? undefined : d.toISOString();
}

// actOnMail sends one action the way S06 prefers: it reads the
// candidate afresh, and answers with the token that review issued for
// exactly this action. A revision that moved on is refused here, before
// anything is sent; with no token (no review open yet) it names the
// exact revision instead, which the server checks the same way.
export async function actOnMail(
  row: MailCandidate,
  action: MailCandidateAction,
  fields?: MailCandidateEdit,
): Promise<MailActResult> {
  try {
    const detail = await mailApi.candidate(row.id);
    if (detail.candidate.revision !== row.revision) {
      throw new Error("This event changed since you opened it. Look at the current version and decide again.");
    }
    const token = detail.review?.revision === row.revision ? detail.tokens?.[action] : undefined;
    return await mailApi.actOnCandidate(row.id, token ? { token } : { revision: row.revision }, action, fields);
  } catch (e) {
    if (e instanceof ApiError && e.status === 409) {
      throw new Error(`Nothing was changed: ${e.message}. Look at the current version and decide again.`);
    }
    if (e instanceof ApiError && e.status === 404) {
      throw new Error("This event is no longer waiting for you.");
    }
    if (e instanceof ApiError && e.status === 403) {
      throw new Error("Only you can answer this, from your own session.");
    }
    throw e;
  }
}

const KIND_PILL: Record<MailReviewKind, string> = {
  create: "ready to add",
  update: "change to an added event",
  cancel: "cancelled by the organizer",
};

export function CandidateCard({
  candidate,
  onChanged,
  now,
}: {
  candidate: MailCandidateRow;
  onChanged: () => void;
  now?: Date;
}) {
  const f = candidate.fields;
  const rv = openReview(candidate, now);
  const kind: MailReviewKind = rv?.kind ?? (candidate.updatePending === "cancel" ? "cancel" : "create");
  const actions = actionsFor(candidate, now);
  const held = kind !== "cancel" && needsDetails(candidate);
  const [editing, setEditing] = useState(held && actions.includes("edit"));
  const [draft, setDraft] = useState<MailCandidateEdit>({
    title: f.title,
    start: f.start,
    end: f.end,
    allDay: f.allDay,
    tz: f.tz,
    location: f.location ?? "",
    notes: f.notes ?? "",
  });
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const conflicts = rv?.conflicts ?? [];
  const send = async (action: MailCandidateAction, fields?: MailCandidateEdit) => {
    setBusy(true);
    setError(null);
    setNote(null);
    try {
      const res = await actOnMail(candidate, action, fields);
      setNote(MAIL_OUTCOME_NOTE[res.outcome] ?? res.message);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
      onChanged();
    }
  };

  return (
    <div className="card" aria-label={`event candidate ${f.title || "untitled"}`}>
      <div className="card-head">
        <EnvelopeSimple size={16} aria-hidden />
        <strong className="card-name">{f.title || "Untitled event"}</strong>
        <StatusPill kind={held ? "warn" : kind === "create" ? "ok" : "warn"}>
          {held ? "needs details" : KIND_PILL[kind]}
        </StatusPill>
      </div>
      <div>{eventWhen(f)}</div>
      {f.location && <div className="muted">{f.location}</div>}
      {f.notes && <div className="muted">{f.notes}</div>}
      <div className="muted">
        {kind === "cancel"
          ? `Apply removes it from ${rv?.targetCalendar ?? candidate.targetCalendar}; Dismiss keeps it.`
          : kind === "update"
            ? `Apply changes the event already on ${rv?.targetCalendar ?? candidate.targetCalendar}`
            : `Would be added to ${rv?.targetCalendar ?? candidate.targetCalendar}`}
        {kind !== "cancel" && ` · confidence ${Math.round(f.confidence * 100)}%`}
        {rv?.expiresAt ? ` · decide by ${when(rv.expiresAt)}` : candidate.expiresAt ? ` · decide by ${when(candidate.expiresAt)}` : ""}
      </div>
      {held && (
        <div className="callout callout-warn">
          Needs details:{" "}
          {[fieldList(f.missing) && `missing ${fieldList(f.missing)}`, fieldList(f.ambiguous) && `unclear ${fieldList(f.ambiguous)}`]
            .filter(Boolean)
            .join("; ") || candidate.stateReason || "the time is not certain"}
          . Nothing is added, and no conflicts are checked, until you fill them in.
        </div>
      )}
      {!held && kind !== "cancel" && rv && (
        rv.availability === "unavailable" ? (
          <div className="callout callout-warn">
            Your calendars could not be checked for conflicts. They are checked again when you decide.
          </div>
        ) : conflicts.length === 0 ? (
          <div className="muted">
            No conflicts on {(rv.checkedCalendars ?? []).length > 0 ? (rv.checkedCalendars ?? []).join(", ") : "the calendars you chose"}.
          </div>
        ) : (
          <div className="callout callout-warn">
            Conflicts:
            <ul>
              {conflicts.map((c, i) => (
                <li key={i}>
                  {c.title || "busy"} on {c.calendar}
                  {c.start ? ` — ${c.allDay ? new Date(c.start).toLocaleDateString() : when(c.start)}` : ""}
                </li>
              ))}
            </ul>
          </div>
        )
      )}
      {editing && (
        <div style={{ display: "grid", gap: "var(--space-2)", marginTop: "var(--space-3)" }}>
          <div className="field">
            <label htmlFor={`cand-title-${candidate.id}`}>Title</label>
            <input id={`cand-title-${candidate.id}`} value={draft.title} onChange={(e) => setDraft({ ...draft, title: e.target.value })} />
          </div>
          <label>
            <input type="checkbox" checked={draft.allDay} onChange={(e) => setDraft({ ...draft, allDay: e.target.checked })} /> All day
          </label>
          <div className="field">
            <label htmlFor={`cand-start-${candidate.id}`}>Starts</label>
            <input
              id={`cand-start-${candidate.id}`}
              type="datetime-local"
              value={toLocalInput(draft.start)}
              onChange={(e) => setDraft({ ...draft, start: fromLocalInput(e.target.value) })}
            />
          </div>
          <div className="field">
            <label htmlFor={`cand-end-${candidate.id}`}>Ends</label>
            <input
              id={`cand-end-${candidate.id}`}
              type="datetime-local"
              value={toLocalInput(draft.end)}
              onChange={(e) => setDraft({ ...draft, end: fromLocalInput(e.target.value) })}
            />
          </div>
          <div className="field">
            <label htmlFor={`cand-loc-${candidate.id}`}>Place</label>
            <input id={`cand-loc-${candidate.id}`} value={draft.location} onChange={(e) => setDraft({ ...draft, location: e.target.value })} />
          </div>
        </div>
      )}
      <div className="row">
        {actions.includes("add") && (
          <button className="btn btn-primary" disabled={held || busy} onClick={() => send("add")}>
            Add
          </button>
        )}
        {!actions.includes("add") && kind === "create" && actions.length > 0 && (
          <button className="btn btn-primary" disabled>
            Add
          </button>
        )}
        {actions.includes("apply") && (
          <button className="btn btn-primary" disabled={held || busy} onClick={() => send("apply")}>
            Apply
          </button>
        )}
        {actions.includes("edit") &&
          (editing ? (
            <button className="btn btn-secondary" disabled={!draft.title.trim() || !draft.start || busy} onClick={() => send("edit", draft)}>
              Save details
            </button>
          ) : (
            <button className="btn btn-secondary" onClick={() => setEditing(true)}>
              Edit
            </button>
          ))}
        {actions.includes("dismiss") && (
          <button className="btn btn-ghost" disabled={busy} onClick={() => send("dismiss")}>
            Dismiss
          </button>
        )}
      </div>
      {note && <div className="msg-note">{note}</div>}
      <ErrorLine error={error} />
    </div>
  );
}

// ---------------------------------------------------------------- owner

// The household's monitors, for an Owner: who watches, whether each is
// waiting on a decision, and how it is doing. Approving happens in Plans
// and only there — this links to it and decides nothing itself.
export function HouseholdMonitors({
  subs,
  pending,
  onNavigate,
}: {
  subs: AgentSubscription[];
  pending: PlanRecord[];
  onNavigate: Navigate;
}) {
  return (
    <section className="work-section" aria-label="household mailbox monitors">
      <div className="sect">
        <span className="eyebrow">Household monitors</span>
        <span className="muted">Owners only</span>
        <span className="fade" />
      </div>
      {pending.length > 0 && (
        <div className="callout callout-warn">
          {pending.length} mailbox {pending.length === 1 ? "decision is" : "decisions are"} waiting for an Owner.{" "}
          <button className="btn btn-secondary" onClick={() => onNavigate("plans")}>
            Review in Plans
          </button>
        </div>
      )}
      {subs.length === 0 ? (
        <div className="muted">Nobody watches a mailbox yet.</div>
      ) : (
        <ul className="work-evidence">
          {subs.map((s) => (
            <li key={s.id}>
              <strong>{s.person ?? "unknown person"}</strong>
              <span className="timeline-src">{s.source}</span>
              <span className="timeline-src">{s.agent}</span>
              <StatusPill kind={!s.enabled ? "mute" : s.awaitingApproval ? "warn" : s.schedule?.hold ? "warn" : "ok"}>
                {!s.enabled
                  ? "off"
                  : s.awaitingApproval
                    ? "waiting for Owner approval"
                    : s.schedule?.hold
                      ? s.schedule.hold.replace("_", " ")
                      : (s.schedule?.lastOutcome ?? "not run yet")}
              </StatusPill>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
