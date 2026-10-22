import { useState } from "react";
import { Cpu } from "@phosphor-icons/react";
import { api, BlockDevice, JoinTokenResponse, ListeningPort, NodeInfo } from "../api";
import { ago, duration, ErrorLine, useLoad, when } from "../lib";
import { nodeKey, useConsoleState } from "../state";
import { Dot, Kind, StatusPill } from "../ui/status";
import { Sparkline } from "../ui/Sparkline";

// Nodes (ADR-W016 A2 / ADR-W014 A3 / E10-U2): pairing plus the
// onboarding validation probes of scope §7.2 — mint the join token,
// then check the new node's storage, ports, and reachability live. The
// Restore path shares this screen (it branches after pairing, per the
// scope's "do not bolt this on later").
export function Nodes() {
  const [minted, setMinted] = useState<JoinTokenResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const fleet = useLoad(() => api.nodes());
  const { snap } = useConsoleState();
  const nodes = fleet.data?.nodes ?? [];

  const mint = () => {
    setBusy(true);
    setError(null);
    api
      .mintJoinToken()
      .then(setMinted)
      .catch((e: Error) => setError(e.message))
      .finally(() => setBusy(false));
  };

  return (
    <>
      <Inventory
        nodes={nodes}
        error={fleet.error}
        loading={fleet.loading}
        reload={fleet.reload}
        history={snap?.history}
      />
      <div className="sect" style={{ marginTop: "var(--space-8)" }}>
        <span className="eyebrow">Pairing</span>
        <span className="fade" />
      </div>
      <div className="sub">
        Pair a new machine: mint a single-use join token, run the install command on it as root,
        then validate it below. The token is shown exactly once.
      </div>
      <div className="row">
        <button className="btn btn-primary" disabled={busy} onClick={mint}>
          Mint join token
        </button>
      </div>
      <ErrorLine error={error} />
      {minted && (
        <div className="card">
          <h3>Pairing code (valid {Math.round(minted.expiresInSec / 60)} min)</h3>
          <pre>{minted.install}</pre>
          {minted.caFingerprint && (
            <div className="muted">CA fingerprint (pins the join): {minted.caFingerprint}</div>
          )}
        </div>
      )}
      <Probes nodes={nodes} />
      <div className="card">
        <h3>Restoring an existing home?</h3>
        <div className="muted">
          The Restore path branches here (scope §7.2 step 3): pair the fresh node above as usual,
          set the capability&apos;s restic repository password on it (Secrets →{" "}
          <code>restic-&lt;capability&gt;</code>), attach or restore the repository volume, then
          use Backups → Restore alongside to bring the latest snapshot into a scratch volume and
          move data in deliberately. In-place restore stays an explicit High-risk plan
          (ADR-W011/W012).
        </div>
      </div>
    </>
  );
}

// Inventory (#30, ADR-W027): the fleet as cards. Until #30 no screen in
// the product could answer "what nodes do I have, and are they
// connected?" — this view offered a join-token button and a probe panel
// driven by a node name you had to already know and type in by hand.
//
// Uptime is the numeral because it is the question: a node that has been
// up for eleven days and one that reconnected ninety seconds ago are in
// very different states, and both said "connected".
export function Inventory({
  nodes,
  error,
  loading,
  reload,
  history,
}: {
  nodes: NodeInfo[];
  error: string | null;
  loading: boolean;
  reload: () => void;
  history?: Map<string, Kind[]>;
}) {
  return (
    <>
      <div className="sect">
        <span className="eyebrow">Fleet</span>
        <span className="muted">
          {nodes.filter((n) => n.connected).length} of {nodes.length} connected
        </span>
        <span className="fade" />
        <button className="btn btn-ghost" disabled={loading} onClick={reload}>
          Refresh
        </button>
      </div>
      <ErrorLine error={error} />
      {!error && nodes.length === 0 && !loading && (
        <div className="muted">
          No nodes have joined yet. Mint a join token below and run the install command on the
          machine you want to add.
        </div>
      )}
      <div className="tiles">
        {nodes.map((n) => (
          <NodeCard key={n.id} node={n} samples={history?.get(nodeKey(n.id)) ?? []} />
        ))}
      </div>
    </>
  );
}

export function NodeCard({ node, samples }: { node: NodeInfo; samples: Kind[] }) {
  const kind: Kind = node.connected ? "ok" : "bad";
  return (
    <div className="tile" style={{ cursor: "default" }}>
      <div className="tile-head">
        <Cpu size={16} />
        <span className="tile-name">{node.id}</span>
      </div>
      <div style={{ display: "flex", alignItems: "baseline", gap: 6 }}>
        <span className="num" style={{ fontSize: 26 }}>
          {duration(node.connected ? node.connectedSince : node.lastSeenAt)}
        </span>
        <span className="unit">{node.connected ? "connected" : "since last seen"}</span>
      </div>
      <div className="tile-foot">
        <Dot kind={kind} pulse={node.connected} />
        <StatusPill kind={kind}>{node.connected ? "connected" : "disconnected"}</StatusPill>
      </div>
      <Sparkline samples={samples} height={12} label={`the last ${samples.length} checks`} />
      <div className="tile-meta">
        <span>agent {node.agentVersion || "unknown"}</span>
        {node.protocolMinor !== undefined && <span>proto minor {node.protocolMinor}</span>}
      </div>
      <div className="tile-meta">
        <span title={when(node.firstJoinedAt)}>joined {ago(node.firstJoinedAt)}</span>
      </div>
    </div>
  );
}

// Probes: the live validation checks — every underlying agent op is
// dial-or-read only.
function Probes({ nodes }: { nodes: NodeInfo[] }) {
  const [node, setNode] = useState("");
  const [target, setTarget] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<string | null>(null);
  const [devices, setDevices] = useState<BlockDevice[] | null>(null);
  const [ports, setPorts] = useState<ListeningPort[] | null>(null);

  const run = (fn: () => Promise<void>) => {
    setBusy(true);
    setError(null);
    fn()
      .catch((e: Error) => setError(e.message))
      .finally(() => setBusy(false));
  };

  const dial = (kind: "nfs" | "smb" | "http") =>
    run(async () => {
      const r =
        kind === "http"
          ? await api.probeHttp(node, target)
          : await api.probeDial(node, kind, target);
      setResult(
        `${kind} ${target}: ${r.reachable ? "reachable" : "NOT reachable"}${r.detail ? ` — ${r.detail}` : ""}`,
      );
    });

  return (
    <div className="card">
      <h3>Validation probes</h3>
      <div className="row">
        {nodes.length > 0 ? (
          <select value={node} onChange={(e) => setNode(e.target.value)}>
            <option value="">select a node…</option>
            {nodes.map((n) => (
              <option key={n.id} value={n.id}>
                {n.id}
                {n.connected ? "" : " (disconnected)"}
              </option>
            ))}
          </select>
        ) : (
          <input
            type="text"
            placeholder="node id (e.g. n1)"
            value={node}
            onChange={(e) => setNode(e.target.value)}
          />
        )}
        <button
          className="btn btn-secondary"
          disabled={busy || !node}
          onClick={() => run(async () => setDevices((await api.probeBlockDevices(node)).devices))}
        >
          Block devices
        </button>
        <button
          className="btn btn-secondary"
          disabled={busy || !node}
          onClick={() => run(async () => setPorts((await api.probePorts(node)).ports))}
        >
          Listening ports
        </button>
      </div>
      <div className="row">
        <input
          type="text"
          placeholder="target: host (nfs/smb) or url (http)"
          value={target}
          onChange={(e) => setTarget(e.target.value)}
        />
        <button className="btn btn-secondary" disabled={busy || !node || !target} onClick={() => dial("nfs")}>
          NFS
        </button>
        <button className="btn btn-secondary" disabled={busy || !node || !target} onClick={() => dial("smb")}>
          SMB
        </button>
        <button className="btn btn-secondary" disabled={busy || !node || !target} onClick={() => dial("http")}>
          HTTP
        </button>
      </div>
      <ErrorLine error={error} />
      {result && <div className="muted">{result}</div>}
      {devices && (
        <div className="chips" style={{ marginTop: 8 }}>
          {devices.length === 0 && <span className="muted">no block devices reported</span>}
          {devices.map((d) => (
            <span className="chip" key={d.name}>
              {d.name} · {(d.sizeBytes / 1e9).toFixed(0)} GB{d.removable ? " · removable" : ""}
            </span>
          ))}
        </div>
      )}
      {ports && (
        <div className="chips" style={{ marginTop: 8 }}>
          {ports.length === 0 && <span className="muted">nothing listening</span>}
          {ports.map((p, i) => (
            <span className="chip" key={i}>
              {p.proto}:{p.port} on {p.localIp}
            </span>
          ))}
        </div>
      )}
    </div>
  );
}
