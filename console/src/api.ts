// The Console's ONLY fetch path (ADR-W016 A3): base URL + bearer token,
// JSON in/out, wire shapes typed by hand against the API (whose tests
// pin them server-side). Resource specs come typed from the generated
// schema (ADR-W007 A7).

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
  node?: string;
  yaml: string;
}

export interface Observed {
  // Not converged because a plan is waiting for a human, not because
  // anything failed. Rendered differently from a real problem.
  awaitingApproval?: boolean;
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
    throw new ApiError(res.status, (data as { error?: string }).error ?? res.statusText);
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
  transport: string;
  account: string;
  surface: ButlerSurface;
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
  surface: ButlerSurface;
  identity: string;
  transport?: string;
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

// A Keystone principal. An admin-surface binding names one: the chat
// account speaks as that principal, so the invite picks from the ones
// that exist rather than trusting a typed name.
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
  surface: ButlerSurface;
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

export interface ButlerModel {
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

export const api = {
  packages: () => call<PackageView[]>("GET", "/api/v1/packages"),
  butlerModels: (endpoint?: string) =>
    call<{ endpoint: string; models: ButlerModel[] }>(
      "GET",
      `/api/v1/butler/models${endpoint ? `?endpoint=${encodeURIComponent(endpoint)}` : ""}`,
    ),
  butlerConfig: () => call<{ stored: boolean; config: ButlerConfig }>("GET", "/api/v1/butler/config"),
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
  disableCapability: (capability: string, owned: { kind: string; name: string }[]) =>
    call<PlanRecord>("POST", "/api/v1/plans", {
      summary: `disable capability ${capability} (${owned.length} resource(s) removed)`,
      delete: owned.map((r) => ({ kind: r.kind, name: r.name })),
    }),
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
  butlerSessions: (limit = 20, surface?: ButlerSurface) =>
    call<ButlerSession[]>(
      "GET",
      `/api/v1/butler/sessions?limit=${limit}${surface ? `&surface=${surface}` : ""}`,
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
  issueChatClaim: (surface: ButlerSurface, identity: string, transport?: string) =>
    call<ChatClaim>("POST", "/api/v1/chat/claims", {
      surface,
      identity,
      ...(transport ? { transport } : {}),
    }),
  revokeChatBinding: (id: number) =>
    call<unknown>("POST", `/api/v1/chat/bindings/${id}/revoke`),
  chatTransports: () => call<ChatTransport[]>("GET", "/api/v1/chat/transports"),
  // Gives a chat capability its Keystone token and places it on the
  // node as the secret the adapter container reads. Minting again is
  // rotation: earlier tokens are revoked first.
  mintChatAdapter: (capability: string, node: string, surface: ButlerSurface) =>
    call<{ principal: string; secret: string; node: string }>(
      "POST",
      `/api/v1/chat/adapters/${encodeURIComponent(capability)}`,
      { node, surface },
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
