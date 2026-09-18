// The Console's ONLY fetch path (ADR-W016 A3): base URL + bearer token,
// JSON in/out, wire shapes typed by hand against the API (whose tests
// pin them server-side). Resource specs come typed from the generated
// schema (ADR-W007 A7).

import type { AutomationSpec } from "./generated/resources";

// ---------------------------------------------------------------- wire shapes

export interface Identity {
  name: string;
  role: string;
  actingFor?: string;
}

export interface ResourceView {
  kind: string;
  name: string;
  owner: string;
  // Which package produced this. Owner names the CAPABILITY, and two
  // profiles of one capability share it, so this is what tells a
  // removal which resources are the named package's. Absent on
  // resources written before the field existed.
  package?: string;
  node?: string;
  yaml: string;
}

export interface Observed {
  // Not converged because a plan is waiting for a human, not because
  // anything failed. Rendered differently from a real problem.
  awaitingApproval?: boolean;
  // The container's OWN healthcheck, verbatim from Docker ("starting",
  // "healthy", "unhealthy"). Absent means the image declares none, which
  // is no opinion — never a problem.
  appHealth?: string;
  kind: string;
  name: string;
  node?: string;
  healthy: boolean;
  detail?: string;
  updatedAt: string;
}

export interface EventRow {
  seq: number;
  time: string;
  type: string;
  node?: string;
  kind?: string;
  name?: string;
  message?: string;
  attrs?: Record<string, string>;
}

export interface PlanRecord {
  seq: number;
  id: string;
  createdAt: string;
  principal: string;
  actingFor?: string;
  summary: string;
  risk: string;
  state: string;
  basedOnHead?: string;
  plan?: Plan;
  resolvedBy?: string;
  resolvedAt?: string;
}

// Plan mirrors keystone/plan.Plan as the API now serves it: steps with
// named risk and action, not ordinals, and no JSON-inside-JSON.
export interface Plan {
  id?: string;
  summary?: string;
  steps?: PlanStep[];
  overallRisk?: string;
}

export interface PlanStep {
  description?: string;
  kind?: string;
  name?: string;
  action?: string;
  risk?: string;
  blastRadius?: string;
}

export interface BackupRun {
  seq: number;
  planName: string;
  runKind: string;
  planId?: string;
  startedAt: string;
  finishedAt?: string;
  status: string;
  detail?: string;
}

export interface SecretInfo {
  name: string;
  version: number;
  createdUnixSec?: number;
  updatedUnixSec?: number;
}

// NodeInfo is one row of the fleet inventory (#30). connected comes from
// the gateway's live session registry, never from stored state.
export interface NodeInfo {
  id: string;
  connected: boolean;
  agentVersion?: string;
  protocolMinor?: number;
  firstJoinedAt: string;
  lastSeenAt: string;
  connectedSince?: string;
}

export interface BlockDevice {
  name: string;
  sizeBytes: number;
  removable: boolean;
}

export interface ListeningPort {
  proto: string;
  port: number;
  localIp: string;
}

export interface ProbeResult {
  reachable: boolean;
  detail?: string;
}

export interface JoinTokenResponse {
  joinToken: string;
  caFingerprint?: string;
  expiresInSec: number;
  install: string;
}

// ---------------------------------------------------------------- client

const TOKEN_KEY = "wildstone-token";

export function getToken(): string | null {
  return sessionStorage.getItem(TOKEN_KEY);
}
export function setToken(token: string) {
  sessionStorage.setItem(TOKEN_KEY, token);
}
export function clearToken() {
  sessionStorage.removeItem(TOKEN_KEY);
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    // The session a failed Butler turn left its work in, when there is
    // one.
    //
    // A failed request normally has nothing but a message. A turn that
    // failed AFTER running tools is the exception: some of those calls
    // changed the house, the server keeps the durable task and names
    // the session holding it, and a caller that drops the name retries
    // into a fresh task and runs them again. So it travels on the error
    // rather than being parsed out of the body at each call site, where
    // one of them would eventually forget.
    public sessionId?: string,
    // Proposals the turn tried to make and the queue refused.
    //
    // These travel for the same reason sessionId does, and are the more
    // urgent of the two. A plan that WAS created is visible on the
    // Plans screen and in the thread; a refused one exists nowhere.
    // ResourceYAML never reaches the stored transcript, so resuming the
    // task cannot submit it either — the operator has to ask again, and
    // will not know to unless this is shown.
    public proposalErrors?: string[],
  ) {
    super(message);
  }
}

// onUnauthorized is installed by the app shell: a 401 anywhere returns
// to the token gate (ADR-W016 A3).
export let onUnauthorized: () => void = () => {};
export function setOnUnauthorized(fn: () => void) {
  onUnauthorized = fn;
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: {
      Authorization: `Bearer ${getToken() ?? ""}`,
      ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401) {
    clearToken();
    onUnauthorized();
    throw new ApiError(401, "unauthenticated");
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const failed = data as { error?: string; sessionId?: string; proposalErrors?: string[] };
    throw new ApiError(
      res.status,
      failed.error ?? res.statusText,
      failed.sessionId,
      failed.proposalErrors,
    );
  }
  return data as T;
}

// PackageView is what the catalog serves (ADR-W021 A5). Params travel
// with required/default because they ARE the install form: a package
// declares what it needs, and this screen asks for exactly that.
export interface ParamDecl {
  name: string;
  required?: boolean;
  default?: string;
  // What the field MEANS, in words someone who does not know the
  // internals can act on (ADR-W021 A5).
  description?: string;
  // What the DEPLOYMENT already knows. options means "these are the
  // only sensible answers"; suggested means "this is almost certainly
  // it" — a fact about this deployment, not about the package.
  options?: string[];
  suggested?: string;
  // Which deployment fact this param is sourced from, when it is one.
  // "directory" makes the form offer a folder picker instead of a blank
  // box (ADR-W021 A5).
  source?: string;
  // What this capability is ACTUALLY running with, when it is already
  // installed. It wins over suggested and default, because those are
  // answers for a FIRST install and this is the only one that is true of
  // the running service. Without it, reconfiguring silently reset every
  // param the operator did not retype.
  installed?: string;
  // A syntactically valid value, written by the package author (ADR-W025
  // A4). It shows the SHAPE and nothing else — it is not a suggestion for
  // this deployment and never becomes the field's value, only its
  // placeholder of last resort. "lint-listen-addr" is what the toolchain
  // invents without one, and an operator staring at an empty box labelled
  // listen_addr has the same problem the example exists to solve.
  example?: string;
}

export interface ServiceAddress {
  route: string;
  url: string;
  healthy: boolean;
  detail?: string;
}

export interface ResourceRef {
  kind: string;
  name: string;
  // A Secret's one-line explanation, when its package declares one.
  purpose?: string;
}

export interface PackageView {
  name: string;
  version: string;
  capability: string;
  profile?: string;
  params?: ParamDecl[];
  // The secrets only an operator can supply, as this package declares
  // them. They are not derivable from a convention — the Telegram
  // adapter reads <capability>-bot and the Matrix one <capability>-token
  // — so a form that collects one must ask for these names.
  providedSecrets?: string[];
  // The app's own logo, as a dashboard-icons slug. Resolved by Keystone
  // from the table the household dashboard renders from, so the Console
  // never keeps its own name-to-slug map — a second copy of that table
  // is a copy that drifts.
  icon?: string;
  provides?: { trait: string; mode?: string }[];
  requires?: { trait: string; mode?: string }[];
  enabled: boolean;
  // Where an installed capability is served, whether it answers, what it
  // created, and what stands in the way of it working here.
  // The version this capability is actually running, and whether the
  // catalog now holds a different one.
  installedVersion?: string;
  stale?: boolean;
  addresses?: ServiceAddress[];
  resources?: ResourceRef[];
  blockers?: string[];
}

// EnablePreview is a dry run: the plan a proposal WOULD make, and the
// secrets only the operator can supply. Nothing is recorded.
export interface EnablePreview {
  plan: Plan;
  providedSecrets: string[];
  // What installing this does to how EVERYONE signs in. Present only
  // when it raises the requirement for services already installed.
  authPolicyWarning?: string;
}

// --- Butler (ADR-W017). The status endpoint says whether a provider is
// wired and what it resolved to; an unconfigured Butler answers 200 with
// a reason, so the surface can teach rather than just fail.
export interface ButlerStatus {
  configured: boolean;
  // Set on a per-Butler status: which Butler this describes.
  butler?: string;
  provider?: string;
  model?: string;
  endpoint?: string;
  mode?: string;
  reason?: string;
  tools?: string[];
}

export interface AskResponse {
  sessionId: string;
  mode: string;
  finalText: string;
  capped?: boolean;
  // Plans the session proposed. They are in state `proposed` — a human
  // approves them on the Plans screen, never here.
  planIds?: string[];
  proposalErrors?: string[];
}

export interface ButlerSession {
  seq: number;
  id: string;
  createdAt: string;
  principal: string;
  actingFor: string;
  provider: string;
  mode: string;
  // Which butler answered (ADR-W026 A1). Stamped by the endpoint the
  // session arrived on, never by the caller.
  surface: ButlerSurface;
  // Which named Butler held the conversation (ADR-W031 A1). Empty on a
  // session that ran before Butlers existed, or one the engine phrased
  // for itself — a real value, not a gap to fill in.
  butler?: string;
  // The LAST turn's answer and transcript, kept for every reader that
  // predates turns. A session is a conversation now (ADR-W027).
  finalText: string;
  capped: boolean;
  transcriptJson?: string;
  // Set by GET /butler/sessions/{id}, omitted from the list.
  turns?: ButlerTurn[];
}

// One message in a session. The tool traffic is not a turn: it is
// recorded inside the assistant turn it produced, so "which reads
// produced THIS answer" stays answerable once a session has answered
// more than once.
export interface ButlerTurn {
  ordinal: number;
  role: "user" | "assistant";
  text: string;
  createdAt: string;
  capped?: boolean;
  transcriptJson?: string;
  planIds?: string[];
}

// The two surfaces (ADR-W026 A1): the operator's butler reads system
// state and proposes plans; the household's reads the household timeline
// and performs domestic actions, with no access to the home lab itself.
export type ButlerSurface = "admin" | "family";

// --- Chat bindings (ADR-W026 A4). A binding is the only way a chat
// account reaches a Butler, and it exists only because somebody redeemed
// a claim code.
export interface ChatBinding {
  id: number;
  connection: number;
  // The Agent whose bot this account is admitted to, and the transport
  // that bot speaks. Both come from the Connection rather than being
  // stored beside it, so they cannot disagree with it.
  butler: string;
  transport: string;
  account: string;
  identity: string;
  displayName?: string;
  createdAt: string;
  createdBy: string;
  lastUsedAt?: string;
  revokedAt?: string;
}

// The code comes back exactly once: the server stores only its hash.
export interface ChatClaim {
  code: string;
  butler: string;
  identity: string;
  transport: string;
  expiresAt: string;
  note: string;
  // The bot to send it to, and a link that opens a chat with it. Both
  // are known only once that adapter has checked in. On Telegram the
  // link CARRIES the code, so tapping it is the whole invite.
  handle?: string;
  link?: string;
}

// --- Household members (ADR-W020 A3, ADR-W023 A7). A member IS an
// `LdapUser` in the Statefile: the household's account list lives in
// state, in plans, in approval and in audit, rather than in LLDAP's own
// UI where a restore would bring back every group with nobody in them.
export interface HouseholdMember {
  username: string;
  displayName?: string;
  email?: string;
  groups: string[];
  // "system" for someone an operator added, "capability/<name>" for an
  // account a package asserts.
  owner: string;
  node: string;
}

// A Keystone principal — a machine credential, never a person. Chat
// invites deliberately do NOT pick from these: a Connection admits a
// Person (ADR-W033 A2/B9), and a chat account bound to a credential had
// nobody behind it.
export interface Principal {
  name: string;
  role: string;
  actingFor?: string;
  disabled?: boolean;
  createdAt: string;
}

export interface HouseholdGroup {
  name: string;
  owner: string;
  node: string;
}

// --- Chat transports (ADR-W026 A3). What an invite may name: the
// adapters this deployment actually has, not a free-text guess.
export interface ChatTransport {
  transport: string;
  // The Agent this bot answers for, absent until it has been
  // credentialed and given an address. A bot belongs to exactly one
  // Agent (ADR-W033 B7); the surface this used to carry named a class
  // of them and could not tell one from another.
  butler?: string;
  capability: string;
  installed: boolean;
  // Whether the adapter has its Keystone token. Installed without one
  // means the bot is running and cannot reach the control plane, so an
  // invite issued for it can never be redeemed.
  credentialed: boolean;
  // The bot itself, as the adapter reported it — a Telegram username, a
  // Matrix MXID. Empty until one has checked in; nothing else in the
  // deployment knows it, because the bot token is write-only.
  handle?: string;
  seenAt?: string;
  // Whether that check-in was recent enough to mean the adapter is
  // running now. A bot nobody has messaged and a bot that is down look
  // identical without this.
  online: boolean;
  // Where an installed adapter runs, from the resources its capability
  // owns. Rotating its credential or writing its bot token must target
  // THIS node: doing either on another one revokes the running
  // adapter's token and places the replacement where nothing reads it.
  // Empty when the capability's resources span nodes.
  node?: string;
}

export interface IdentityNotification {
  code?: string;
  body?: string;
  note: string;
}

export interface OfferedModel {
  name: string;
  sizeBytes?: number;
  parameters?: string;
  // Whether this runs on your hardware or is proxied to a hosted
  // backend — the difference between a question staying in the house
  // and leaving it.
  local: boolean;
}

export interface ButlerConfig {
  provider: string;
  model?: string;
  endpoint?: string;
  probeMode?: string;
  probeDetail?: string;
  updatedAt?: string;
  updatedBy?: string;
}

// DirectoryListing is what the folder picker browses. Roots are the only
// places a service's data can live; a path outside them is refused by
// the node at apply time, so the picker never offers one.
export interface DirectoryListing {
  path?: string;
  parent?: string;
  roots: string[];
  directories: string[];
}


// ---------------------------------------------------------------- durable work

// A Butler task is one unit of durable work: a conversational request,
// or one occurrence of a routine (ADR-W030 A4). Unlike a session, it
// survives a restart, so the Console can show what is still running,
// what is waiting and on what, and what was interrupted.
export interface ButlerTask {
  id: string;
  kind: "conversation" | "routine";
  state:
    | "pending"
    | "running"
    | "waiting"
    | "completed"
    | "failed"
    | "cancelled"
    | "paused-offboarded";
  author: string;
  owner: string;
  surface: string;
  profile: string;
  audience: "private" | "shared";
  objective: string;
  completionCriteria: string;
  resultSummary?: string;
  uncertainty?: string;
  sessionId?: string;
  createdAt: string;
  updatedAt: string;
  deadlineAt?: string;
  nextWakeupAt?: string;
  leaseEpoch: number;
}

// One row of the work trace as the API renders it (ADR-W030 A11).
// Detail carries counts and flags — never what a source SAID, which
// belongs to the source and is read from there.
export interface TimelineEntry {
  at: string;
  type: string;
  step?: number;
  attempt?: number;
  actor?: string;
  sources?: string[];
  modelEndpoint?: string;
  manifestVersion?: string;
  promptVersion?: string;
  policyVersion?: string;
  detail?: Record<string, unknown>;
  traceSeq: number;
}

export interface TaskStep {
  seq: number;
  ordinal: number;
  kind: string;
  name: string;
  state: string;
}

export interface TaskWait {
  seq: number;
  createdAt: string;
  reason: string;
  detail?: string;
  resumeAt?: string;
  resolvedAt?: string;
  resolvedBy?: string;
}

export interface TaskEvidence {
  seq: number;
  recordedAt: string;
  kind: string;
  description: string;
  sourceId?: string;
  audience: string;
}

export interface TaskTransition {
  at: string;
  fromState: string;
  toState: string;
  reason?: string;
  actor?: string;
}

// The labels a person may put on a task. Closed, and the same set the
// store enforces — the value of a label is that it can be counted.
export const FEEDBACK_LABELS = [
  "wrong-answer",
  "stale-read",
  "repeated-work",
  "inappropriate-tool",
  "permission-refusal",
  "excessive-interruption",
  "useful-completion",
] as const;
export type FeedbackLabel = (typeof FEEDBACK_LABELS)[number];

export interface TaskFeedback {
  seq: number;
  createdAt: string;
  author: string;
  label: FeedbackLabel;
  note?: string;
  traceSeq?: number;
}

export interface TaskDetail {
  task: ButlerTask;
  timeline: TimelineEntry[];
  steps: TaskStep[];
  waits: TaskWait[];
  evidence: TaskEvidence[];
  transitions: TaskTransition[];
  feedback: TaskFeedback[];
  // How many entries this reader is not shown. Reported rather than
  // silently dropped: an incomplete story presented as complete is
  // worse than one that says what is missing.
  withheld: number;
  debugActive: boolean;
}

export interface ButlerEvaluation {
  seq: number;
  createdAt: string;
  author: string;
  name: string;
  sourceTaskId?: string;
  summary: string;
  expected: string;
  promptVersion?: string;
  policyVersion?: string;
  manifestVersion?: string;
  modelEndpoint?: string;
  evaluatorVersion?: string;
  fixtureJson?: string;
}

export interface ButlerSettingsView {
  householdRounds: number;
  operatorRounds: number;
  maxToolCalls: number;
  stallSeconds: number;
  retryLimit: number;
  observationTtlSeconds: number;
  quietStartMin: number;
  quietEndMin: number;
  timezone: string;
  updatedBy?: string;
}


// ---------------------------------------------------------------- tool authority

// An administrator's switch for one profile and tool (ADR-W030 A7). A
// control can turn an existing capability off and on; it can never add
// one, because the surface's tool map is the structural ceiling.
export interface ToolControl {
  profile: string;
  tool: string;
  state: "enabled" | "disabled";
  updatedAt: string;
  updatedBy: string;
  reason?: string;
}

// The Butler asking for something it does not have. A request grants
// nothing: approving it records a decision, and enabling the tool is a
// separate act on purpose.
export interface CapabilityRequest {
  seq: number;
  kind: "supported-tool" | "development";
  requestedBy: string;
  profile: string;
  tool?: string;
  description: string;
  state: "open" | "approved" | "declined" | "implemented";
  createdAt: string;
  decidedAt?: string;
  decidedBy?: string;
  decisionNote?: string;
}

// Butler admission, which is runtime state and not LDAP membership.
export interface ButlerMemberRecord {
  member: string;
  profile: string;
  admitted: boolean;
  generation: number;
  guardian?: string;
  disabledAt?: string;
  offboardedAt?: string;
}

export interface ButlerIdentityLink {
  seq: number;
  linkType: string;
  externalId: string;
  member: string;
  createdBy: string;
  revokedAt?: string;
}


// ---------------------------------------------------------------- memory

// What the Butler remembers, as owned data with provenance (ADR-W030
// A3). Every record says where it came from, so it can be corrected at
// its source and invalidated when that source is deleted.
export interface MemoryRecord {
  seq: number;
  kind: "fact" | "preference" | "inference" | "procedure";
  subject?: string;
  content: string;
  audience: "private" | "shared";
  owner: string;
  sourceKind: string;
  sourceId: string;
  confidence: string;
  safetyRelevant?: boolean;
  createdAt: string;
  author: string;
  supersededBy?: number;
  forgottenAt?: string;
}

export interface MemoryDigestEntry {
  seq: number;
  memory: number;
  change: "recorded" | "corrected" | "forgotten";
  summary: string;
  actor: string;
  at: string;
  immediate: boolean;
}


// One recorded retrieval fixture run (ADR-W030 P9). The implementation
// choice is measured rather than asserted, and this is the measurement.
export interface IndexMeasurement {
  seq: number;
  at: string;
  fixture: string;
  strategy: "keyword" | "vector" | "hybrid";
  queries: number;
  hits: number;
  recallPct: number;
  p95Micros: number;
  indexBytes: number;
  note?: string;
}


// ---------------------------------------------------------------- integrations

// One named connection to an application (ADR-W030 A5). Keystone holds
// the contract and the leases; the application-specific half lives in an
// adapter outside it.
export interface IntegrationConnection {
  seq: number;
  name: string;
  adapter: string;
  endpoint?: string;
  ownership: "personal" | "household";
  owner: string;
  audience: "private" | "shared";
  actionsEnabled: boolean;
  indexingEnabled: boolean;
  cloudAllowed: boolean;
  state: "configured" | "ready" | "degraded" | "unavailable";
  leaseEpoch: number;
  degradedReason?: string;
}

export interface IntegrationCapability {
  kind: "event" | "query" | "action";
  name: string;
  version?: string;
  available: boolean;
  detail?: string;
}

// Where an adapter may run. A node is eligible only once all three
// facts hold — the container is running, the secret is provisioned, and
// readiness was verified — because each fails separately.
export interface Placement {
  node: string;
  role: "primary" | "fallback";
  rank: number;
  containerRunning: boolean;
  secretProvisioned: boolean;
  readinessVerified: boolean;
  holder: boolean;
  removing?: boolean;
  containerRemoved?: boolean;
  secretErased?: boolean;
  credentialRevoked?: boolean;
}

// A reference to a credential, never the credential.
export interface ConnectionSecret {
  seq: number;
  member?: string;
  secretRef: string;
  upstreamReadOnly: boolean;
  disclosure?: string;
  revokedAt?: string;
}


// ---------------------------------------------------------------- model routes

// Where a question may be sent (ADR-W030 A9). A route's kind is the data
// boundary: cloud means material sent to it leaves the house, and a
// route cannot change kind.
// One named model a Butler can be assigned to: which model, where it
// runs, and whether asking it something leaves the house.
//
// The bundle exists rather than a bare model string on the Butler
// because none of its parts implies the others. One Ollama endpoint
// serves gemma4:e4b off local disk and gemma4:31b-cloud from Ollama's
// own infrastructure, so neither the endpoint nor the provider says
// where the material goes — kind does.
export interface Model {
  name: string;
  kind: "local" | "cloud";
  provider: string;
  providerModel?: string;
  endpoint?: string;
  readiness: "unprobed" | "ready" | "qa-only" | "unavailable";
  readinessDetail?: string;
  // When something last asked this model to operate a tool. Absent
  // means nobody ever has, which is not the same as it having failed.
  probedAt?: string;
  enabled: boolean;
}

// A Butler: a named specialist with one assigned model, its own
// directive, and its own scope (ADR-W031 A1).
//
// The model is a property of the Butler rather than a per-call choice.
// Which model saw a piece of material is therefore answerable by name,
// which is the whole reason routing was removed (A2).
export interface Butler {
  name: string;
  directive?: string;
  surface: ButlerSurface;
  profile: string;
  // model is the assigned model, by name. providerModel is what the
  // provider itself calls it, which is a different and often more
  // specific string: assigned "kitchen-box", answered by "qwen3.5:4b".
  model: string;
  modelKind: "local" | "cloud";
  providerModel?: string;
  provider?: string;
  endpoint?: string;
  readiness: "unprobed" | "ready" | "qa-only" | "unavailable";
  // The MODEL's switch, not the Butler's: turning a model off has to
  // stop the Butlers already assigned to it.
  modelEnabled: boolean;
  enabled: boolean;
  createdAt: string;
  createdBy: string;
  updatedAt: string;
  updatedBy?: string;
  retiredAt?: string;
}

// The model as a person would name it.
//
// A Butler's configured model has a NAME that is a stable key and a
// providerModel that is what the provider calls it. For anything new
// they are the same string, because the model's own name is used. They
// differ on rows configured before that — "kitchen-box" pointing at
// qwen3.5:4b — and everywhere a screen shows a model to a person it
// should say the model, not the key.
export function modelLabel(b: Butler): string {
  return b.providerModel || b.model;
}

// One roster row. leavesTheHouse is computed by the server rather than
// derived here: it is the most consequential fact about a Butler, and a
// client that derives it is one that can derive it wrongly.
export interface ButlerRow {
  butler: Butler;
  leavesTheHouse: boolean;
  carriesActions: boolean;
  reachable: boolean;
  // How far it reaches, counted by the server. The roster shows one
  // line per Butler saying who can talk to it and what it may do, and a
  // client assembling that would make three calls per row — with
  // nothing to stop one failing and a row quietly reading "no people"
  // for a Butler two people message.
  people: number;
  connections: number;
  // toolsAvailable is what this deployment actually wired for the
  // profile, NOT the whole ceiling: "5 of 6" is what this Butler can do
  // here, where "5 of 13" would count tools nobody ever set up.
  toolsAllowed: number;
  toolsAvailable: number;
}

export interface ButlerScope {
  butler: string;
  model: string;
  modelKind: "local" | "cloud";
  connections: string[] | null;
  narrowedTools?: Record<string, string> | null;
  memoryCount: number;
}

// What retiring a Butler would break (ADR-W031 A8). Every number is
// counted by the server, because the dialog that shows it is asking
// somebody to accept a consequence — "some routines may fail" moves the
// work of finding out back onto the person it was meant to inform.
export interface ButlerImpact {
  butler: string;
  people: string[] | null;
  routines: { name: string; state: string }[] | null;
  sessions: number;
  memoryCount: number;
  // Other live Butlers on the same surface: what a routine could be
  // pointed at instead.
  successors: string[] | null;
}

// One tool as the gateway reports it (ADR-W030 P2).
//
// available is separate from presence on purpose, and the reason is
// the useful half: a tool this deployment never wired, and one this
// Butler has been narrowed out of, are both unavailable and only the
// second is something the screen can undo.
export interface CapabilityTool {
  name: string;
  summary: string;
  mutating?: boolean;
  grantRequired?: boolean;
  untrusted?: boolean;
  available: boolean;
  reason?: string;
}

// What one Butler may do, as the engine resolves it. The console does
// not derive any of this: the tool list is the gateway's registry and
// availability is the effective-authority resolver's answer, so a
// screen cannot show a tool as on that a dispatch would refuse.
export interface ButlerTools {
  butler: string;
  profile: string;
  surface: ButlerSurface;
  modelVerdict: string;
  tools: CapabilityTool[] | null;
  actions: { tool: string; tier: string }[] | null;
  // Tool name to the reason it was narrowed, "" when none was given.
  narrowedTools: Record<string, string> | null;
}

export interface ButlerAddress {
  transport: string;
  externalId: string;
  label?: string;
}

// A routine the household wrote, native to an app or run by Wildstone.
export interface HouseholdRoutine {
  name: string;
  ownership: "native" | "wildstone";
  externalId?: string;
  connection?: string;
  author: string;
  revision: number;
  state: "draft" | "trial" | "enabled" | "disabled";
  document: string;
  createdAt: string;
  updatedAt: string;
  updatedBy: string;
  disabledAt?: string;
  disabledBy?: string;
}

// One recorded edit to a routine. The document is not carried: a
// listing of every past version is a lot of bytes for a card that shows
// three lines each.
export interface RoutineRevision {
  revision: number;
  author: string;
  at: string;
  note: string;
}

// One event on the household stream (ADR-W024 A2). A second stream with
// its own envelope and its own cursor — not the operational one.
export interface HouseholdEvent {
  seq: number;
  eventId: string;
  occurredAt: string;
  recordedAt: string;
  // Absent is valid: an unmapped subject is a real event that simply
  // cannot fire a per-member rule (A8).
  subject?: string;
  // Stamped from the publisher token, never read off the payload (A10).
  source: string;
  type: string;
  entity?: string;
  payload?: Record<string, unknown>;
  correlationId?: string;
  schemaVersion: number;
}

// One routine, with what it does and what it has been doing.
export interface RoutineDetail {
  routine: HouseholdRoutine;
  // The name the ENGINE knows this routine by — the document's
  // metadata.name, which need not be the name it is filed under.
  rule: string;
  // The document parsed server-side, by the same path the engine uses.
  // Null when it will not parse, which is not an error worth hiding —
  // see specError.
  spec: AutomationSpec | null;
  specError?: string;
  revisions: RoutineRevision[];
  // The engine's own records for this rule over the last 7 days:
  // automation.fired / suppressed / acted / action_failed. Not butler
  // tasks — nothing produces a task of kind "routine".
  runs: HouseholdEvent[];
}

// The closed grammar a routine may be written in (ADR-W024 A5), served
// so a form cannot offer a value the validator or the policy engine will
// then refuse. Offering free text here would be worse than offering
// nothing: a routine triggering on an event nothing emits never fires,
// and nothing tells its author why.
// Commitments (ADR-W035). A Standing is where one stands right now: the
// declaration plus the occurrence the engine derived, which is why
// nothing here is stored and everything is recomputed.
export interface CommitmentRule {
  meter: string;
  calendar: boolean;
  anchor: number;
  dueAt: number;
  leadAt: number;
  position: number;
  known: boolean;
  stale: boolean;
  remaining: number;
  progress: number;
  crossed: boolean;
  soon: boolean;
  observedAt?: string;
  anchorTime?: string;
  dueTime?: string;
  leadTime?: string;
  unresolved?: string;
}

export interface CommitmentProjection {
  meter: string;
  at: string;
  ratePerDay: number;
  source: string;
}

export interface CommitmentOccurrence {
  cycle: number;
  state: string;
  openedAt: string;
  trigger?: string;
  rules: CommitmentRule[];
  pastDue?: boolean;
  unresolved?: string[];
  projection?: CommitmentProjection;
  reason: string;
}

export interface CommitmentBooking {
  seq: number;
  commitment: string;
  at: string;
  bookedAt: string;
  bookedBy: string;
  note?: string;
}

export interface CommitmentStanding {
  name: string;
  description: string;
  owner?: string;
  scope?: string;
  mayComplete?: string[];
  notify?: string[];
  disabled?: boolean;
  rollover?: string;
  requiresBooking?: boolean;
  occurrence: CommitmentOccurrence;
  booking?: CommitmentBooking;
}

export interface CommitmentCompletion {
  seq: number;
  commitment: string;
  at: string;
  recordedAt: string;
  recordedBy: string;
  waived?: boolean;
  reason?: string;
  positions?: Record<string, number>;
  // The household event that closed this cycle, for a commitment that
  // declares satisfiedBy. Absent for an answer a person gave.
  externalId?: string;
}

export interface MeterReading {
  seq: number;
  meter: string;
  value: number;
  at: string;
  recordedAt: string;
  recordedBy: string;
  source: string;
  rebase?: boolean;
  prior?: number;
  reason?: string;
}

export interface MeterView {
  name: string;
  description: string;
  unit?: string;
  owner?: string;
  staleAfter?: string;
  estimate?: string;
  builtIn?: boolean;
  latest?: MeterReading;
  stale?: boolean;
}

export interface AutomationGrammar {
  eventTypes: { type: string; package: string; capability?: string; subject?: string; description?: string }[];
  actions: { tool: string; tier: string; package: string; capability?: string }[];
  conditionKinds: string[];
  contextKinds: string[];
  tiers: string[];
  weekdays: string[];
}

// One rule's verdict on one event, from the side-effect-free replay
// (ADR-W024 A7/H3) — the same code path `wild automation replay` and the
// Butler's dry_run_automation tool use.
export interface ReplayDecision {
  rule: string;
  eventSeq: number;
  eventId: string;
  eventType: string;
  subject?: string;
  at: string;
  fired: boolean;
  // Why a suppressed decision stopped, naming the condition; for a fired
  // one, "all conditions held".
  reason: string;
  action?: string;
  tier?: string;
  escalates?: boolean;
}

export interface ReplayResult {
  rule: string;
  from: string;
  to: string;
  draft: boolean;
  enabled: boolean;
  fired: number;
  evaluated: number;
  decisions: ReplayDecision[] | null;
  // What a PROPOSAL of this rule would be refused for. Informational: a
  // replay is not a proposal.
  violations?: { rule?: string; message?: string; resource?: string }[] | null;
}

export const api = {
  packages: () => call<PackageView[]>("GET", "/api/v1/packages"),
  offeredModels: (endpoint?: string) =>
    call<{ endpoint: string; models: OfferedModel[] }>(
      "GET",
      `/api/v1/butler/models/offered${endpoint ? `?endpoint=${encodeURIComponent(endpoint)}` : ""}`,
    ),
  models: () =>
    call<{ models: { model: Model; carriesActions: boolean; leavesTheHouse: boolean }[] | null }>(
      "GET",
      "/api/v1/butler/models",
    ),
  setModelEnabled: (name: string, enabled: boolean) =>
    call<{ enabled: boolean }>(
      "POST",
      `/api/v1/butler/models/${encodeURIComponent(name)}/enabled`,
      { enabled },
    ),
  butlers: () => call<{ butlers: ButlerRow[] | null }>("GET", "/api/v1/butlers"),
  createButler: (b: {
    name: string;
    directive?: string;
    surface: string;
    profile: string;
    // Either an already-configured model by name, or one the provider
    // offers plus where it comes from. The server decides the kind.
    model?: string;
    providerModel?: string;
    provider?: string;
    endpoint?: string;
  }) => call<ButlerRow>("POST", "/api/v1/butlers", b),
  butlerScope: (name: string) =>
    call<{ scope: ButlerScope; people: string[] | null; addresses: ButlerAddress[] | null }>(
      "GET",
      `/api/v1/butlers/${encodeURIComponent(name)}/scope`,
    ),
  // Applies immediately (A8). crossesToCloud is set when this move
  // sends an open conversation's history out of the house on the next
  // message, and warning carries the sentence to show.
  // Assigns a model the provider offers, configuring it here if this
  // house has not used it before. The kind — whether asking it leaves
  // the house — is decided by the server from the provider's own
  // listing, never sent from here.
  assignProviderModel: (name: string, providerModel: string, provider: string, endpoint: string) =>
    call<ButlerRow & { crossesToCloud?: boolean; warning?: string; previousModel?: string }>(
      "PUT",
      `/api/v1/butlers/${encodeURIComponent(name)}/model`,
      { providerModel, provider, endpoint },
    ),
  setButlerModel: (name: string, model: string) =>
    call<ButlerRow & { crossesToCloud?: boolean; warning?: string; previousModel?: string }>(
      "PUT",
      `/api/v1/butlers/${encodeURIComponent(name)}/model`,
      { model },
    ),
  setButlerAccess: (name: string, member: string, allow: boolean) =>
    call<{ people: string[] | null }>(
      "PUT",
      `/api/v1/butlers/${encodeURIComponent(name)}/access`,
      { member, allow },
    ),
  // A directive instructs and authorises nothing (A9), so rewriting one
  // is a plain edit rather than a proposal.
  setButlerDirective: (name: string, directive: string) =>
    call<ButlerRow>("PUT", `/api/v1/butlers/${encodeURIComponent(name)}/directive`, { directive }),
  // Where a Butler answers. On chat this is the bot that receives the
  // message, and it is what decides which Butler replies (A7) — so it
  // is the one field on this screen that changes who hears what.
  setButlerAddress: (
    name: string,
    a: { transport: string; externalId: string; label?: string; allow: boolean },
  ) =>
    call<{ addresses: ButlerAddress[] | null }>(
      "PUT",
      `/api/v1/butlers/${encodeURIComponent(name)}/address`,
      a,
    ),
  retireButler: (name: string) =>
    call<{ retired: string }>("DELETE", `/api/v1/butlers/${encodeURIComponent(name)}`),
  restoreButler: (name: string) =>
    call<ButlerRow>("POST", `/api/v1/butlers/${encodeURIComponent(name)}/restore`, {}),
  // One call, not six. Creating then granting each connection then
  // re-narrowing each tool can half-fail, and the half that fails is
  // the narrowing — leaving a copy quietly wider than its original.
  duplicateButler: (name: string, copyName?: string) =>
    call<ButlerRow & { note?: string }>(
      "POST",
      `/api/v1/butlers/${encodeURIComponent(name)}/duplicate`,
      copyName ? { name: copyName } : {},
    ),
  butlerImpact: (name: string) =>
    call<ButlerImpact>("GET", `/api/v1/butlers/${encodeURIComponent(name)}/impact`),
  // Asks a model whether it can actually operate a tool, and stores the
  // verdict. This is how a model earns the authority to do things — an
  // unprobed one answers questions and nothing more.
  probeModel: (name: string) =>
    call<{ model: Model }>(
      "POST",
      `/api/v1/butler/models/${encodeURIComponent(name)}/probe`,
      {},
    ),
  // What is answering for ONE Butler, and what it may do. The
  // deployment-wide status was one provider, one model, one tool list —
  // true while there was one Butler, and another Butler's answer the
  // moment there were several.
  oneButlerStatus: (name: string) =>
    call<ButlerStatus>("GET", `/api/v1/butlers/${encodeURIComponent(name)}/status`),
  butlerSessionsFor: (name: string, limit = 20) =>
    call<ButlerSession[]>(
      "GET",
      `/api/v1/butler/sessions?butler=${encodeURIComponent(name)}&limit=${limit}`,
    ),
  butlerTools: (name: string) =>
    call<ButlerTools>("GET", `/api/v1/butlers/${encodeURIComponent(name)}/tools`),
  // Narrowing only, and the API says so in both directions: allow:false
  // takes a tool away from this Butler, allow:true gives it back to
  // whatever its profile allows. Neither can widen past the surface's
  // ceiling, which is why this is not a state field.
  setButlerTool: (name: string, tool: string, allow: boolean, reason = "") =>
    call<{ butler: string; narrowedTools: Record<string, string> | null; note?: string }>(
      "PUT",
      `/api/v1/butlers/${encodeURIComponent(name)}/tool`,
      { tool, allow, reason },
    ),
  setButlerConnection: (name: string, connection: string, allow: boolean) =>
    call<{ butler: string; connections: string[] | null }>(
      "PUT",
      `/api/v1/butlers/${encodeURIComponent(name)}/connection`,
      { connection, allow },
    ),
  householdRoutines: () =>
    call<{ routines: HouseholdRoutine[] | null; specs?: Record<string, AutomationSpec> }>(
      "GET",
      "/api/v1/household/routines",
    ),
  // A routine is born a DRAFT whatever is asked for: activation goes
  // through the state endpoint and the Plan boundary, which is where a
  // human is. The form says so rather than letting the server refuse.
  createHouseholdRoutine: (r: { name: string; document: string; ownership?: string }) =>
    call<{ seq: number; note?: string }>("POST", "/api/v1/household/routines", {
      name: r.name,
      document: r.document,
      ownership: r.ownership ?? "wildstone",
      externalId: "",
      connection: "",
      state: "",
    }),
  householdRoutine: (name: string) =>
    call<RoutineDetail>("GET", `/api/v1/household/routines/${encodeURIComponent(name)}`),
  // The household stream (ADR-W024 A2). Its own cursor, and the handler
  // normalises an empty stream to [] rather than null.
  householdEvents: (after: number, limit = 100) =>
    call<HouseholdEvent[]>(
      "GET",
      `/api/v1/household/events?after=${after}&limit=${limit}`,
    ),
  automationGrammar: () =>
    call<AutomationGrammar>("GET", "/api/v1/household/automations/grammar"),
  // Exactly one of: name (an approved rule), rule (a draft document), or
  // routine+spec (a draft held as a parsed spec, which is what the form
  // has).
  commitments: () => call<CommitmentStanding[]>("GET", "/api/v1/household/commitments"),
  commitment: (name: string) =>
    call<{ standing: CommitmentStanding; completions: CommitmentCompletion[] }>(
      "GET",
      `/api/v1/household/commitments/${encodeURIComponent(name)}`,
    ),
  meters: () => call<MeterView[]>("GET", "/api/v1/household/meters"),
  meterReadings: (name: string) =>
    call<MeterReading[]>("GET", `/api/v1/household/meters/${encodeURIComponent(name)}/readings`),
  recordReading: (name: string, value: number) =>
    call<{ seq: number; duplicate: boolean }>(
      "POST",
      `/api/v1/household/meters/${encodeURIComponent(name)}/readings`,
      { value },
    ),

  replayAutomation: (req: {
    name?: string;
    rule?: string;
    routine?: string;
    spec?: AutomationSpec;
    limit?: number;
  }) => call<ReplayResult>("POST", "/api/v1/household/automations/replay", req),
  // The spec goes back as the structured form the detail read served,
  // because the browser has no YAML writer and should not grow one: the
  // server owns this grammar in both directions.
  editRoutine: (name: string, spec: AutomationSpec, note: string, basedOnRevision: number) =>
    call<{ revision: number; state: string; note: string }>(
      "POST",
      `/api/v1/household/routines/${encodeURIComponent(name)}/edit`,
      { spec, note, basedOnRevision },
    ),
  setRoutineState: (name: string, state: string) =>
    call<{ routine: HouseholdRoutine; performsActions: boolean }>(
      "POST",
      `/api/v1/household/routines/${encodeURIComponent(name)}/state`,
      { state },
    ),
  integrations: () =>
    call<{
      connections: { connection: IntegrationConnection; capabilities: IntegrationCapability[] | null }[] | null;
    }>("GET", "/api/v1/integrations"),
  integrationPlacement: (name: string) =>
    call<{ placements: Placement[] | null; secrets: ConnectionSecret[] | null }>(
      "GET",
      `/api/v1/integrations/${encodeURIComponent(name)}/placement`,
    ),
  placeIntegration: (name: string) =>
    call<{ holder: string | null; moved?: boolean; waiting?: boolean; note?: string }>(
      "POST",
      `/api/v1/integrations/${encodeURIComponent(name)}/placement/place`,
      {},
    ),
  widenIntegration: (name: string, w: { actions: boolean; indexing: boolean; cloud: boolean }) =>
    call<{ widened: string }>("POST", `/api/v1/integrations/${encodeURIComponent(name)}/widen`, w),
  indexMeasurements: () =>
    call<{ measurements: IndexMeasurement[] | null }>("GET", "/api/v1/butler/index/measurements"),
  compactButlerIndex: (maxAgeDays: number) =>
    call<{ prunedForgotten: number; agedOut: number; note: string }>(
      "POST",
      "/api/v1/butler/index/compact",
      { maxAgeDays },
    ),
  // butler narrows to one Butler's material; omitting it is every
  // Butler the reader may see, which is what the endpoint defaults to.
  butlerMemory: (q?: { subject?: string; butler?: string }) => {
    const params = new URLSearchParams();
    if (q?.subject) params.set("subject", q.subject);
    if (q?.butler) params.set("butler", q.butler);
    const qs = params.toString();
    return call<{ records: MemoryRecord[] | null }>(
      "GET",
      `/api/v1/butler/memory${qs ? `?${qs}` : ""}`,
    );
  },
  correctButlerMemory: (seq: number, content: string) =>
    call<{ seq: number }>("POST", "/api/v1/butler/memory/correct", {
      seq,
      content,
      confidence: "stated",
    }),
  forgetButlerMemory: (seq: number) =>
    call<{ note: string }>("POST", "/api/v1/butler/memory/forget", { seq }),
  memoryDigest: () =>
    call<{ entries: MemoryDigestEntry[] | null }>("GET", "/api/v1/butler/memory/digest"),
  butlerToolControls: (profile?: string) =>
    call<{ controls: ToolControl[] | null }>(
      "GET",
      `/api/v1/butler/tools${profile ? `?profile=${encodeURIComponent(profile)}` : ""}`,
    ),
  setButlerToolControl: (c: { profile: string; tool: string; state: string; reason: string }) =>
    call<{ note?: string }>("PUT", "/api/v1/butler/tools", c),
  capabilityRequests: (state?: string) =>
    call<{ requests: CapabilityRequest[] | null }>(
      "GET",
      `/api/v1/butler/requests${state ? `?state=${encodeURIComponent(state)}` : ""}`,
    ),
  decideCapabilityRequest: (seq: number, state: string, note: string) =>
    call<{ note: string }>("POST", "/api/v1/butler/requests/decide", { seq, state, note }),
  butlerMembers: () =>
    call<{ members: { member: ButlerMemberRecord; links: ButlerIdentityLink[] | null; active: boolean }[] | null }>(
      "GET",
      "/api/v1/butler/members",
    ),
  // Recording and admitting are separate acts on purpose: existing and
  // being allowed to use the Butler are different facts, and the panel
  // says so. This is the first half, which had no control at all.
  recordButlerMember: (member: string, profile: string, guardian?: string) =>
    call<{ recorded: string }>("POST", "/api/v1/butler/members", {
      member,
      profile,
      guardian: guardian ?? "",
    }),
  admitButlerMember: (member: string) =>
    call<unknown>("POST", `/api/v1/butler/members/${encodeURIComponent(member)}/admit`, {}),
  disableButlerMember: (member: string) =>
    call<unknown>("POST", `/api/v1/butler/members/${encodeURIComponent(member)}/disable`, {}),
  offboardButlerMember: (member: string) =>
    call<{ pausedTasks: number }>(
      "POST",
      `/api/v1/butler/members/${encodeURIComponent(member)}/offboard`,
      {},
    ),
  butlerConfig: () => call<{ stored: boolean; config: ButlerConfig }>("GET", "/api/v1/butler/config"),
  butlerSettings: () =>
    call<{ stored: boolean; settings: ButlerSettingsView }>("GET", "/api/v1/butler/settings"),
  setButlerSettings: (s: Partial<ButlerSettingsView>) =>
    call<{ stored: boolean; settings: ButlerSettingsView }>("PUT", "/api/v1/butler/settings", s),
  butlerTasks: () => call<{ tasks: ButlerTask[] | null }>("GET", "/api/v1/butler/tasks"),
  butlerTask: (id: string) =>
    call<TaskDetail>("GET", `/api/v1/butler/tasks/${encodeURIComponent(id)}`),
  labelButlerTask: (id: string, label: FeedbackLabel, note: string, traceSeq?: number) =>
    call<{ recorded: boolean }>("POST", `/api/v1/butler/tasks/${encodeURIComponent(id)}/feedback`, {
      label,
      note,
      traceSeq: traceSeq ?? 0,
    }),
  exportButlerEvaluation: (
    id: string,
    body: { name: string; expected: string; summary: string; evaluatorVersion: string },
  ) =>
    call<{ seq: number; name: string }>(
      "POST",
      `/api/v1/butler/tasks/${encodeURIComponent(id)}/evaluation`,
      body,
    ),
  butlerEvaluations: () =>
    call<{ evaluations: ButlerEvaluation[] | null }>("GET", "/api/v1/butler/evaluations"),
  setButlerConfig: (c: { provider: string; model: string; endpoint: string }) =>
    call<{ stored: boolean; config: ButlerConfig }>("PUT", "/api/v1/butler/config", c),
  // The install form's folder picker. Read-only, and the node answers
  // only for the prefixes a bind mount may name — so what it offers is
  // exactly what will actually work.
  directories: (node: string, path?: string) =>
    call<DirectoryListing>(
      "GET",
      `/api/v1/nodes/${encodeURIComponent(node)}/directories${path ? `?path=${encodeURIComponent(path)}` : ""}`,
    ),
  identityCode: (node: string) =>
    call<IdentityNotification>("GET", `/api/v1/nodes/${encodeURIComponent(node)}/identity/code`),
  previewCapability: (pkg: string, params: Record<string, string>) =>
    call<EnablePreview>("POST", "/api/v1/capabilities", {
      package: pkg,
      params,
      dryRun: true,
    }),
  enableCapability: (pkg: string, params: Record<string, string>) =>
    call<PlanRecord>("POST", "/api/v1/capabilities", { package: pkg, params }),
  whoami: () => call<Identity>("GET", "/api/v1/whoami"),
  resources: () => call<ResourceView[]>("GET", "/api/v1/resources"),
  observed: () => call<Observed[]>("GET", "/api/v1/observed"),
  events: (after: number, limit = 100) =>
    call<EventRow[]>("GET", `/api/v1/events?after=${after}&limit=${limit}`),
  plans: () => call<PlanRecord[]>("GET", "/api/v1/plans"),
  // Removing a capability is an ordinary plan proposing the deletion of
  // everything it owns. There is no disable endpoint and there should
  // not be one: a side door around the human gate is what ground rule 4
  // exists to prevent. `wild capability disable` builds this same body
  // from the same two calls.
  // Removal names a PACKAGE and Keystone decides what that means.
  //
  // This used to build the delete list here, from every resource owned
  // by `capability/<name>`. With two profiles of one capability
  // installed that took the sibling's containers and routes with it —
  // and `Secret/restic-books`, the repository password the surviving app
  // still needs. Every protection added server-side was bypassed by the
  // screen most people use. The catalog and the recorded install params
  // are what answer "what would the sibling still need", and only the
  // control plane has them.
  disablePackage: (pkg: string) =>
    call<PlanRecord>("POST", "/api/v1/capabilities/disable", { package: pkg }),
  // The same computation without proposing anything, so the preview is
  // the plan rather than a second guess at it. Filtering by package
  // stamp here still disagreed with the server: the surviving sibling's
  // needs are excluded there and cannot be known from the stamps alone,
  // so Secret/restic-books was listed as deletable and then wasn't
  // deleted.
  previewDisablePackage: (pkg: string) =>
    call<PlanRecord>("POST", "/api/v1/capabilities/disable", { package: pkg, dryRun: true }),
  plan: (id: string) => call<PlanRecord>("GET", `/api/v1/plans/${encodeURIComponent(id)}`),
  approvePlan: (id: string) =>
    call<unknown>("POST", `/api/v1/plans/${encodeURIComponent(id)}/approve`),
  rejectPlan: (id: string) =>
    call<unknown>("POST", `/api/v1/plans/${encodeURIComponent(id)}/reject`),
  // Each surface has its own endpoint rather than a mode flag, so the
  // Console cannot accidentally send a household question to the
  // operator's butler or the reverse.
  butlerStatus: (surface: ButlerSurface = "admin") =>
    call<ButlerStatus>("GET", surface === "family" ? "/api/v1/household/butler" : "/api/v1/butler"),
  askButler: (message: string, surface: ButlerSurface = "admin") =>
    call<AskResponse>(
      "POST",
      surface === "family" ? "/api/v1/household/butler/ask" : "/api/v1/butler/ask",
      { message },
    ),
  // Asks ONE named Butler, on whichever surface it lives.
  //
  // Deliberately its own route rather than askButler with a butler
  // field: the surface endpoints are doors that pin a ceiling, so
  // /butler/ask refuses a household Butler and says "no butler answers
  // there for you". This one takes the ceiling from the Butler's own
  // record, which is the only correct answer for a caller who holds
  // both surfaces (ADR-W031 A10).
  askOneButler: (name: string, message: string) =>
    call<AskResponse>("POST", `/api/v1/butlers/${encodeURIComponent(name)}/ask`, { message }),
  butlerSessions: (limit = 20, surface?: ButlerSurface, butler?: string) =>
    call<ButlerSession[]>(
      "GET",
      `/api/v1/butler/sessions?limit=${limit}${surface ? `&surface=${surface}` : ""}` +
        (butler ? `&butler=${encodeURIComponent(butler)}` : ""),
    ),
  butlerSession: (id: string) =>
    call<ButlerSession>("GET", `/api/v1/butler/sessions/${encodeURIComponent(id)}`),
  // A follow-up. There is one endpoint rather than one per surface
  // because the surface comes from the stored session — continuing a
  // household conversation cannot move it onto the operator's Butler.
  continueButlerSession: (id: string, message: string) =>
    call<AskResponse>("POST", `/api/v1/butler/sessions/${encodeURIComponent(id)}/ask`, { message }),
  chatBindings: (withRevoked = false) =>
    call<ChatBinding[]>("GET", `/api/v1/chat/bindings${withRevoked ? "?revoked=true" : ""}`),
  // An invite admits one Person to one Agent's bot (ADR-W033 B7). It
  // used to name a surface, which is a class of Agents: an invite to
  // `admin` admitted somebody to every Owner Agent at once.
  // connection is the bot's capability name. It is always sent, not
  // only when it is needed: an agent may hold several bots on one
  // transport (ADR-W033 B7), and this screen already knows which row
  // the operator picked, so there is nothing for the server to guess.
  issueChatClaim: (butler: string, identity: string, transport: string, connection: string) =>
    call<ChatClaim>("POST", "/api/v1/chat/claims", { butler, identity, transport, connection }),
  revokeChatBinding: (id: number) =>
    call<unknown>("POST", `/api/v1/chat/bindings/${id}/revoke`),
  chatTransports: () => call<ChatTransport[]>("GET", "/api/v1/chat/transports"),
  // Gives a chat capability its Keystone token, places it on the node
  // as the secret the adapter container reads, and assigns the bot to
  // an Agent — one act, because two would leave a credentialed bot
  // answering for nobody. A bot already serving another Agent is
  // refused rather than repointed. Minting again is rotation.
  mintChatAdapter: (capability: string, node: string, butler: string) =>
    call<{ principal: string; secret: string; node: string; butler: string }>(
      "POST",
      `/api/v1/chat/adapters/${encodeURIComponent(capability)}`,
      { node, butler },
    ),
  principals: () => call<Principal[]>("GET", "/api/v1/principals"),
  householdMembers: () => call<HouseholdMember[]>("GET", "/api/v1/household/members"),
  householdGroups: () => call<HouseholdGroup[]>("GET", "/api/v1/household/groups"),
  // Adding a member proposes a plan; the account exists once a human
  // approves it on the Plans screen.
  addHouseholdMember: (m: {
    username: string;
    displayName: string;
    email: string;
    groups: string[];
    node?: string;
  }) => call<PlanRecord>("POST", "/api/v1/household/members", m),
  // Editing is a proposal like adding is: the account list belongs in
  // state, in plans, in approval and in audit. Groups may only be ADDED
  // — the provisioning job never removes a membership, so a plan that
  // dropped one would apply and change nothing in the directory.
  updateHouseholdMember: (
    username: string,
    m: { displayName?: string; email?: string; groups: string[] },
  ) => call<PlanRecord>("PUT", `/api/v1/household/members/${encodeURIComponent(username)}`, m),
  backups: () => call<{ runs: BackupRun[] }>("GET", "/api/v1/backups"),
  runBackup: (plan: string) =>
    call<unknown>("POST", `/api/v1/backups/${encodeURIComponent(plan)}/run`),
  restoreBackup: (plan: string) =>
    call<unknown>("POST", `/api/v1/backups/${encodeURIComponent(plan)}/restore`),
  probeBlockDevices: (node: string) =>
    call<{ devices: BlockDevice[] }>(
      "GET",
      `/api/v1/nodes/${encodeURIComponent(node)}/probes/blockdevices`,
    ),
  probePorts: (node: string) =>
    call<{ ports: ListeningPort[] }>(
      "GET",
      `/api/v1/nodes/${encodeURIComponent(node)}/probes/ports`,
    ),
  probeHttp: (node: string, url: string) =>
    call<ProbeResult>(
      "GET",
      `/api/v1/nodes/${encodeURIComponent(node)}/probes/http?url=${encodeURIComponent(url)}`,
    ),
  probeDial: (node: string, kind: "nfs" | "smb", host: string) =>
    call<ProbeResult>(
      "GET",
      `/api/v1/nodes/${encodeURIComponent(node)}/probes/${kind}?host=${encodeURIComponent(host)}`,
    ),
  nodes: () => call<{ nodes: NodeInfo[] }>("GET", "/api/v1/nodes"),
  mintJoinToken: (ttlSeconds?: number) =>
    call<JoinTokenResponse>("POST", "/api/v1/join-tokens", ttlSeconds ? { ttlSeconds } : {}),
  nodeSecrets: (node: string) =>
    call<{ node: string; secrets: SecretInfo[] }>(
      "GET",
      `/api/v1/nodes/${encodeURIComponent(node)}/secrets`,
    ),
  setSecret: (node: string, name: string, value?: string) =>
    call<unknown>(
      "POST",
      `/api/v1/nodes/${encodeURIComponent(node)}/secrets/${encodeURIComponent(name)}`,
      value ? { value } : {},
    ),
  rotateSecret: (node: string, name: string) =>
    call<unknown>(
      "POST",
      `/api/v1/nodes/${encodeURIComponent(node)}/secrets/${encodeURIComponent(name)}/rotate`,
    ),
};
