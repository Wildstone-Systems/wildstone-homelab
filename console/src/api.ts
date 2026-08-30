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

export const api = {
  whoami: () => call<Identity>("GET", "/api/v1/whoami"),
  resources: () => call<ResourceView[]>("GET", "/api/v1/resources"),
  observed: () => call<Observed[]>("GET", "/api/v1/observed"),
  events: (after: number, limit = 100) =>
    call<EventRow[]>("GET", `/api/v1/events?after=${after}&limit=${limit}`),
  plans: () => call<PlanRecord[]>("GET", "/api/v1/plans"),
  plan: (id: string) => call<PlanRecord>("GET", `/api/v1/plans/${encodeURIComponent(id)}`),
  approvePlan: (id: string) =>
    call<unknown>("POST", `/api/v1/plans/${encodeURIComponent(id)}/approve`),
  rejectPlan: (id: string) =>
    call<unknown>("POST", `/api/v1/plans/${encodeURIComponent(id)}/reject`),
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
