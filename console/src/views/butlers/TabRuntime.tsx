import { useEffect, useState } from "react";
import { Warning } from "@phosphor-icons/react";
import {
  api,
  isPendingStepUp,
  type ButlerRow,
  type ExecutionProfileRef,
  type NodeInfo,
  type RuntimeCredential,
  type RuntimeRecord,
  type RuntimeType,
} from "../../api";
import { ErrorLine } from "../../lib";
import { Confirm } from "../../ui/Confirm";
import { IssuedCredential } from "../Butler";
import { Say } from "./ButlerDrawer";

// Attaching a runtime to an Agent, and setting its execution profile
// (ADR-W039 §3.3, §5; issue #350/H4). This is the Owner-only
// editor; the Butler screen's popover (RuntimeCard, in Butler.tsx) stays
// the read/rotate/revoke view and is not duplicated here.
//
// Two independent settings, and the copy says so twice: runtime TYPE is
// which framework reasons for the Agent, and EXECUTION PROFILE is what
// its own workspace may do. Choosing Hermes never changes the profile;
// choosing a profile never changes what the runtime is (design doc §5).
//
// Saving is a PUT of the whole record (setButlerRuntime), which is
// consequential whenever the type changes — the old credential is
// revoked — or the profile moves to host-admin. Both get the same
// Confirm-then-step-up treatment RuntimeCard gives rotate and revoke;
// a plain reconfiguration (a new endpoint, say) applies at once.
//
// The record and the node list arrive as PROPS, not a self-fetch: the
// drawer already loads them the way it loads scope, tools and impact
// for the other tabs, and a prop is what the render check can hand this
// component directly without a network.
const RUNTIME_TYPES: { key: RuntimeType; title: string; blurb: string; experimental?: boolean }[] = [
  {
    key: "native",
    title: "Wildstone Native",
    blurb: "Reasons in-process, inside Keystone. No endpoint, no separate container.",
  },
  {
    key: "hermes",
    title: "Hermes",
    blurb: "An external Hermes gateway — packaged as a Wildstone capability, or run elsewhere.",
  },
  {
    key: "openclaw",
    title: "OpenClaw",
    blurb: "An external OpenClaw gateway, attached the same way Hermes is.",
    experimental: true,
  },
];

const EXECUTION_PROFILES: { key: ExecutionProfileRef; title: string; blurb: string }[] = [
  {
    key: "platform-only",
    title: "Platform only",
    blurb: "The Agent's ordinary tools — reads, proposes, converses. Nothing inside the runtime's own workspace.",
  },
  {
    key: "sandboxed",
    title: "Sandboxed workspace",
    blurb: "Adds a shell and files inside the runtime's own container. A shell adds no household authority — it can still only call what the Agent may call.",
  },
  {
    key: "host-admin",
    title: "Host administration",
    blurb: "Adds the host capability family to the Agent's manifest — reading logs and status, and administering services — behind the same approval gate as any other action.",
  },
];

interface Draft {
  type: RuntimeType;
  endpoint: string;
  delivery: "push" | "pull";
  processingClass: "" | "local_only" | "cloud";
  executionProfileRef: ExecutionProfileRef;
}

const BLANK_DRAFT: Draft = {
  type: "native",
  endpoint: "",
  delivery: "push",
  processingClass: "",
  executionProfileRef: "platform-only",
};

function fromRecord(rt: RuntimeRecord): Draft {
  return {
    type: rt.type,
    endpoint: rt.endpoint ?? "",
    delivery: rt.negotiated?.delivery ?? "push",
    processingClass: rt.negotiated?.processingClass ?? "",
    executionProfileRef: (rt.executionProfileRef as ExecutionProfileRef) || "platform-only",
  };
}

function sameDraft(a: Draft, rt: RuntimeRecord): boolean {
  const b = fromRecord(rt);
  return (
    a.type === b.type &&
    a.endpoint === b.endpoint &&
    a.delivery === b.delivery &&
    a.processingClass === b.processingClass &&
    a.executionProfileRef === b.executionProfileRef
  );
}

export function TabRuntime({
  row,
  runtime,
  loading,
  error: loadError,
  nodes,
  canManage,
  say,
  onChanged,
}: {
  row: ButlerRow;
  runtime: RuntimeRecord | null;
  loading: boolean;
  error: string | null;
  nodes: NodeInfo[];
  // Attaching a runtime and setting its execution profile are Owner-only
  // (ADR-W033 A4.1, like rotating or revoking a credential): anyone else
  // sees the record and no controls.
  canManage: boolean;
  say: Say;
  onChanged: () => void;
}) {
  const b = row.butler;
  const runtimePath = `/api/v1/butlers/${encodeURIComponent(b.name)}/runtime`;
  const credentialPath = `/api/v1/butlers/${encodeURIComponent(b.name)}/runtime/credential`;

  // Seeded from the loaded record, once per Agent: switching to another
  // Agent while this tab is open must not keep showing the last one's
  // draft, and a background reload after Save must not clobber whatever
  // the person is mid-typing. Assigning state during render, guarded so
  // it runs exactly once per (Agent, record) pair, is React's own escape
  // hatch for resetting derived state when a prop changes.
  const [seededFor, setSeededFor] = useState<string | null>(null);
  const [draft, setDraft] = useState<Draft>(BLANK_DRAFT);
  const seedKey = runtime ? `${b.name}:${JSON.stringify(fromRecord(runtime))}` : null;
  if (runtime && seedKey !== seededFor) {
    setSeededFor(seedKey);
    setDraft(fromRecord(runtime));
  }

  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [confirmSave, setConfirmSave] = useState(false);
  const [offerCredential, setOfferCredential] = useState(false);
  const [node, setNode] = useState("");
  const [issuing, setIssuing] = useState(false);
  const [issueError, setIssueError] = useState<string | null>(null);
  const [issued, setIssued] = useState<RuntimeCredential | null>(null);
  const [upgrading, setUpgrading] = useState(false);
  const [upgradeError, setUpgradeError] = useState<string | null>(null);

  // Back from a step-up: the confirmation, or the credential offer, that
  // was interrupted reopens rather than leaving the person to find it
  // again (as RuntimeCard does for rotate and revoke).
  useEffect(() => {
    if (isPendingStepUp("PUT", runtimePath)) setConfirmSave(true);
    else if (isPendingStepUp("POST", credentialPath)) setOfferCredential(true);
  }, [runtimePath, credentialPath]);

  const dirty = runtime ? !sameDraft(draft, runtime) : false;
  const typeChanged = runtime ? draft.type !== runtime.type : false;
  const movingToHostAdmin =
    draft.executionProfileRef === "host-admin" &&
    (!runtime || (runtime.executionProfileRef || "platform-only") !== "host-admin");
  // Pull-only delivery needs no endpoint: Wildstone never dials the
  // runtime, so there is nothing to reach it AT (ADR-W039 §4.1 "who calls whom").
  const endpointNeeded = draft.type !== "native" && draft.delivery !== "pull";
  const endpointMissing = endpointNeeded && !draft.endpoint.trim();
  // Mirrors the server's own gate, so the Owner sees why before they
  // press Save rather than after.
  const hostAdminBlocked =
    draft.executionProfileRef === "host-admin" && draft.type === "native" && b.agentClass !== "owner";

  async function save() {
    setBusy(true);
    setError(null);
    try {
      const rec = await api.setButlerRuntime(b.name, {
        type: draft.type,
        endpoint: draft.type === "native" ? "" : draft.endpoint.trim(),
        negotiated: { delivery: draft.delivery, processingClass: draft.processingClass || undefined },
        executionProfileRef: draft.executionProfileRef,
      });
      say(
        typeChanged ? `${b.name}'s runtime is now ${rec.type}` : `${b.name}'s runtime saved`,
        typeChanged ? "warn" : "ok",
      );
      const wasNative = runtime?.type === "native";
      onChanged();
      if (typeChanged && wasNative && rec.type !== "native") {
        setOfferCredential(true);
      }
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
      setConfirmSave(false);
    }
  }

  function onSaveClick() {
    if (typeChanged || movingToHostAdmin) {
      setConfirmSave(true);
    } else {
      void save();
    }
  }

  // A newer package version reaches the running runtime only through a
  // restart Plan (#389): the profile controls below re-render only when
  // they change, and the Packages screen re-expands the installed version.
  const pkg = runtime?.package;
  const upgradeTo =
    pkg?.availableVersion && pkg.availableVersion !== pkg.installedVersion ? pkg.availableVersion : "";

  async function upgradePackage() {
    setUpgrading(true);
    setUpgradeError(null);
    try {
      const out = await api.upgradeButlerRuntime(b.name);
      say(`Upgrade to ${out.to} filed as a Plan — approve it to restart ${b.name}'s runtime`, "ok");
      onChanged();
    } catch (e) {
      setUpgradeError((e as Error).message);
    } finally {
      setUpgrading(false);
    }
  }

  async function issueCredential() {
    setIssuing(true);
    setIssueError(null);
    try {
      setIssued(await api.rotateRuntimeCredential(b.name, node || undefined));
      onChanged();
    } catch (e) {
      setIssueError((e as Error).message);
    } finally {
      setIssuing(false);
    }
  }

  if (loading && !runtime) {
    return <div className="tab-stack muted">Checking the runtime…</div>;
  }

  return (
    <div className="tab-stack">
      <ErrorLine error={loadError} />

      {!canManage && (
        <div className="note">
          Only an Owner can change {b.name}'s runtime{runtime ? ` — it runs on ${runtime.type}` : ""}.
        </div>
      )}

      {pkg && (
        <div>
          <div className="eyebrow">PACKAGE</div>
          <div className="row">
            <span>
              Installed from <code>{pkg.name}</code> {pkg.installedVersion}
              {upgradeTo && <span className="muted"> · {upgradeTo} is available</span>}
            </span>
            {canManage && upgradeTo && (
              <button
                type="button"
                className="btn btn-secondary"
                disabled={upgrading}
                onClick={() => void upgradePackage()}
              >
                {upgrading ? "Filing…" : `Upgrade to ${upgradeTo}`}
              </button>
            )}
          </div>
          {upgradeTo && (
            <div className="note">
              Upgrading files a Plan that re-renders this runtime at {upgradeTo} and restarts it. Nothing
              changes until the Plan is approved.
            </div>
          )}
          <ErrorLine error={upgradeError} />
        </div>
      )}

      <div>
        <div className="eyebrow">RUNTIME</div>
        <div className="surface-cards">
          {RUNTIME_TYPES.map((t) => (
            <button
              key={t.key}
              type="button"
              aria-pressed={draft.type === t.key}
              disabled={!canManage}
              className={`surface-card${draft.type === t.key ? " on" : ""}`}
              onClick={() => setDraft((d) => ({ ...d, type: t.key }))}
            >
              <strong>
                {t.title}
                {t.experimental && <span className="muted"> · experimental</span>}
              </strong>
              <span className="muted">{t.blurb}</span>
            </button>
          ))}
        </div>
        <div className="note">
          Changing runtime type revokes {b.name}'s current credential. People, tools, connections,
          routines and memory are untouched.
        </div>
      </div>

      {draft.type !== "native" && (
        <div>
          <label htmlFor="runtime-endpoint">Endpoint</label>
          <input
            id="runtime-endpoint"
            value={draft.endpoint}
            disabled={!canManage}
            placeholder="e.g. container/agent-hermes-mika:8642"
            onChange={(e) => setDraft((d) => ({ ...d, endpoint: e.target.value }))}
          />
          <div className="note">
            For a Wildstone-packaged runtime this is filled in by the package. For a runtime you run
            elsewhere, it is where Wildstone can reach it. Leave it blank if the runtime only pulls
            its deliveries (set delivery to pull, below).
          </div>
          {endpointMissing && (
            <div className="error">An endpoint is needed unless delivery is set to pull.</div>
          )}
        </div>
      )}

      <div className="row">
        <div>
          <label htmlFor="runtime-delivery">Delivery</label>
          <select
            id="runtime-delivery"
            value={draft.delivery}
            disabled={!canManage}
            onChange={(e) => setDraft((d) => ({ ...d, delivery: e.target.value as Draft["delivery"] }))}
          >
            <option value="push">Push — Wildstone delivers to the endpoint</option>
            <option value="pull">Pull — the runtime long-polls for deliveries</option>
          </select>
        </div>
        <div>
          <label htmlFor="runtime-class">Processing class</label>
          <select
            id="runtime-class"
            value={draft.processingClass}
            disabled={!canManage}
            onChange={(e) =>
              setDraft((d) => ({ ...d, processingClass: e.target.value as Draft["processingClass"] }))
            }
          >
            <option value="">Undeclared</option>
            <option value="local_only">Local only — its model runs on your own hardware</option>
            <option value="cloud">Cloud — its model runs off-site</option>
          </select>
        </div>
      </div>
      <div className="note">
        Declared by you, never verified: Wildstone withholds local-only memory from a runtime that
        is not declared local_only.
      </div>

      <div>
        <div className="eyebrow">EXECUTION PROFILE</div>
        <div className="surface-cards">
          {EXECUTION_PROFILES.map((p) => (
            <button
              key={p.key}
              type="button"
              aria-pressed={draft.executionProfileRef === p.key}
              disabled={!canManage}
              className={`surface-card${draft.executionProfileRef === p.key ? " on" : ""}`}
              onClick={() => setDraft((d) => ({ ...d, executionProfileRef: p.key }))}
            >
              <strong>{p.title}</strong>
              <span className="muted">{p.blurb}</span>
            </button>
          ))}
        </div>
        <div className="note">
          A separate setting from runtime type: choosing Hermes never changes the profile, and
          choosing a profile never changes what the runtime is.
        </div>
        {draft.executionProfileRef === "host-admin" && (
          <div className="callout callout-warn">
            <strong>Host administration</strong> lets this Agent read and change your home lab's own
            infrastructure — container logs, service status, restarts — one approved action at a
            time; every write still asks first. It needs an Owner-class Agent. A native Agent gets
            this tier through its own opt-in tools, not through this profile.
          </div>
        )}
        {hostAdminBlocked && (
          <div className="error">
            {b.name} is a Member-class Agent on the native runtime. Host administration here needs a
            non-native runtime or an Owner-class Agent.
          </div>
        )}
      </div>

      <ErrorLine error={error} />

      {canManage && (
        <div className="row">
          <button
            type="button"
            className="btn btn-secondary"
            disabled={busy || !dirty || endpointMissing || hostAdminBlocked}
            onClick={onSaveClick}
          >
            {busy ? "Saving…" : "Save runtime"}
          </button>
          {dirty && !busy && <span className="muted">Unsaved changes</span>}
        </div>
      )}

      {offerCredential && (
        <div className="card">
          <div className="eyebrow">ISSUE A CREDENTIAL</div>
          <div className="note">
            {b.name} has no working credential until one is rotated for its new runtime. Show it
            here, or place it on a node as the provided secret a packaged runtime reads.
          </div>
          <div className="row">
            <select aria-label="Place the credential" value={node} onChange={(e) => setNode(e.target.value)}>
              <option value="">Show it here</option>
              {nodes.map((n) => (
                <option key={n.id} value={n.id}>
                  Place on {n.id}
                </option>
              ))}
            </select>
            <button
              type="button"
              className="btn btn-secondary"
              disabled={issuing}
              onClick={() => void issueCredential()}
            >
              {issuing ? "Issuing…" : "Issue credential"}
            </button>
            <button type="button" className="btn btn-ghost" onClick={() => setOfferCredential(false)}>
              Later
            </button>
          </div>
          <ErrorLine error={issueError} />
          {issued && (
            <IssuedCredential
              credential={issued}
              onDismiss={() => {
                setIssued(null);
                setOfferCredential(false);
              }}
            />
          )}
        </div>
      )}

      {confirmSave && (
        <Confirm
          kind="warn"
          icon={<Warning size={18} weight="bold" />}
          title={typeChanged ? `Move ${b.name} to ${draft.type}?` : "Save this execution profile?"}
          body={
            typeChanged
              ? `${runtime?.credentialRef || "The current credential"} stops working immediately. ${b.name} keeps its people, tools, connections and routines; you issue a credential for the new runtime next.`
              : `${b.name} gains the host capability family the next time its runtime asks for it. Every host write still needs your approval.`
          }
          confirmLabel="Save"
          cancelLabel="Keep it as it is"
          busy={busy}
          onConfirm={() => void save()}
          onCancel={() => setConfirmSave(false)}
        />
      )}
    </div>
  );
}
