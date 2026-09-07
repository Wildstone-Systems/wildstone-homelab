import { useState } from "react";
import {
  api,
  ButlerSurface,
  ChatClaim,
  ChatTransport,
  HouseholdMember,
  NodeInfo,
  PackageView,
  PlanRecord,
  Principal,
  SecretInfo,
} from "../api";
import { ErrorLine, useLoad } from "../lib";

// Connect a chat app (ADR-W026 A2/A3): the steps between a fresh
// deployment and somebody talking to their Butler on a phone.
//
// They were six commands in four places, in an order that matters and
// that nothing enforced — mint the credential, set the bot token,
// install the capability, wait for the container, declare the person,
// invite them. Getting the order wrong does not fail loudly: the
// container starts and 401s forever, or a code is issued for a bot that
// does not exist yet. So this screen READS the state each step leaves
// behind rather than remembering what you clicked, and you can leave and
// come back to it.
//
// The last step is the one that matters to the person being invited: a
// link that opens the bot with the code already in it. Keystone builds
// that link from a handle the adapter reported — never one this screen
// composes, and never one the adapter supplied whole, which would be a
// link the operator trusts and a compromised adapter chose.
export function ConnectChat() {
  const packages = useLoad(() => api.packages());
  const transports = useLoad(() => api.chatTransports());
  const nodes = useLoad(() => api.nodes());
  const [capability, setCapability] = useState("");
  const [node, setNode] = useState("");

  const chatPackages = (packages.data ?? []).filter((p) => p.capability.startsWith("chat-"));
  const chosenPackage = chatPackages.find((p) => p.capability === capability) ?? chatPackages[0];
  const chosen = (transports.data ?? []).find((t) => t.capability === chosenPackage?.capability);
  const nodeList = nodes.data?.nodes ?? [];
  // An installed adapter's node is a FACT, not a choice. Defaulting to
  // the first node in the inventory and offering "Rotate" would revoke
  // the running adapter's token and place the replacement on a machine
  // nothing reads it from, leaving that container permanently
  // unauthorized with no error anywhere.
  const installedNode = chosen?.installed ? chosen.node : "";
  const activeNode = installedNode || node || nodeList[0]?.id || "";

  return (
    <>
      <div className="sub">
        Each surface is its own bot with its own credential — the household bot cannot see the home
        lab, and neither bot can approve anything. Pick which one you are setting up; every step
        below is read from the deployment, so you can leave and come back.
      </div>
      <ErrorLine error={packages.error ?? transports.error ?? nodes.error} />

      <div className="row">
        <select
          value={chosenPackage?.capability ?? ""}
          onChange={(e) => setCapability(e.target.value)}
        >
          {chatPackages.length === 0 && <option value="">no chat packages in the catalog</option>}
          {chatPackages.map((p) => (
            <option key={p.capability} value={p.capability}>
              {p.capability}
            </option>
          ))}
        </select>
        <select
          value={activeNode}
          disabled={installedNode !== ""}
          onChange={(e) => setNode(e.target.value)}
        >
          {nodeList.length === 0 && <option value="">no nodes</option>}
          {nodeList.map((n) => (
            <option key={n.id} value={n.id}>
              {n.id}
              {n.connected ? "" : " (offline)"}
            </option>
          ))}
        </select>
      </div>
      {chosen?.installed && !chosen.node && (
        <div className="muted">
          This capability is installed but its resources span nodes, so this screen cannot say
          which one runs the adapter. Rotate its credential with{" "}
          <code>wild chat adapter {chosen.capability} -node &lt;node&gt; -surface {chosen.surface}</code>{" "}
          rather than from here.
        </div>
      )}
      {installedNode !== "" && (
        <div className="muted">
          Installed on {installedNode}. That is where its credential and bot token live, so it is
          not a choice while it is installed.
        </div>
      )}

      {chosenPackage && activeNode && (
        <Steps
          pkg={chosenPackage}
          node={activeNode}
          nodes={nodeList}
          transport={chosen}
          canAct={!chosen?.installed || installedNode !== ""}
          onChanged={() => transports.reload()}
        />
      )}
    </>
  );
}

// surfaceOf reads the surface out of the capability name — the same
// convention Keystone mints credentials by (chat-<transport>-<surface>),
// and the reason a typo there cannot quietly wire the family's bot to
// the operator's Butler.
export function surfaceOf(capability: string): ButlerSurface {
  return capability.endsWith("-admin") ? "admin" : "family";
}

// vendorSecret is the provided secret the OPERATOR supplies — the bot
// token — as opposed to the one credentialing places (the adapter's own
// Keystone token, always <capability>-adapter).
//
// Read from the package rather than assumed, because the two transports
// disagree: chat-telegram-* declares <capability>-bot and chat-matrix-*
// declares <capability>-token.
export function vendorSecret(pkg: PackageView): string {
  const provided = (pkg.providedSecrets ?? []).filter((s) => s !== `${pkg.capability}-adapter`);
  return provided[0] ?? "";
}

function Steps({
  pkg,
  node,
  nodes,
  transport,
  canAct,
  onChanged,
}: {
  pkg: PackageView;
  node: string;
  nodes: NodeInfo[];
  transport?: ChatTransport;
  // False when the deployment cannot say which node to act on, so the
  // buttons that write to one are withheld rather than guessing.
  canAct: boolean;
  onChanged: () => void;
}) {
  const capability = pkg.capability;
  const secrets = useLoad<{ node: string; secrets: SecretInfo[] }>(
    () => api.nodeSecrets(node),
    [node],
  );
  const members = useLoad<HouseholdMember[]>(() => api.householdMembers());
  const principals = useLoad<Principal[]>(() => api.principals());
  const surface = surfaceOf(capability);
  // The vendor's token, as THIS package declares it: chat-telegram-*
  // reads <capability>-bot and chat-matrix-* reads <capability>-token.
  // Assuming either would write the operator's token into a secret
  // nothing reads and leave the declared one absent, so the reconciler
  // would refuse to start a container while this screen showed the step
  // as done.
  const botSecret = vendorSecret(pkg);
  const tokenSet = botSecret !== "" && (secrets.data?.secrets ?? []).some((s) => s.name === botSecret);

  return (
    <>
      <ErrorLine error={secrets.error} />
      <Step
        n={1}
        title="Make the bot"
        done={false}
        muted
        note={
          surface === "admin"
            ? "Talk to @BotFather and create a bot for YOU. It must be a different bot from the household's: one bot serving both is one stolen phone away from being an infrastructure console."
            : "Talk to @BotFather and create a bot for the HOUSEHOLD. Keep the token it gives you — it is step 3."
        }
      />
      <MintStep
        capability={capability}
        node={node}
        surface={surface}
        done={!!transport?.credentialed}
        canAct={canAct}
        onDone={onChanged}
      />
      <TokenStep
        node={node}
        secret={botSecret}
        done={tokenSet}
        canAct={canAct}
        onDone={secrets.reload}
      />
      <InstallStep pkg={pkg} node={node} nodes={nodes} done={!!transport?.installed} onDone={onChanged} />
      <Step
        n={5}
        title="The bot checks in"
        done={!!transport?.online}
        note={
          transport?.online
            ? `Running as @${transport.handle}. It reported that itself — the bot token is write-only, so nothing else here could know which bot it is.`
            : transport?.handle
              ? `Last seen as @${transport.handle}, but not recently. The container is stopped, or it cannot reach Keystone.`
              : "Nothing has checked in yet. Once the plan applies the container starts and reports its handle within a minute; if it does not, its logs say why — usually a wrong keystone_url or a bad bot token."
        }
      />
      <InviteStep
        surface={surface}
        transport={transport}
        members={members.data ?? []}
        principals={principals.data ?? []}
      />
    </>
  );
}

// Step is one row: a number, a state, and the sentence saying what the
// step is for. A checkmark computed from the deployment is worth more
// than one remembered from a click.
function Step({
  n,
  title,
  done,
  note,
  muted,
  children,
}: {
  n: number;
  title: string;
  done: boolean;
  note?: string;
  muted?: boolean;
  children?: React.ReactNode;
}) {
  return (
    <div className="card">
      <h3>
        {done ? "✓" : muted ? "·" : n} {title}
      </h3>
      {note && <div className="muted">{note}</div>}
      {children}
    </div>
  );
}

function MintStep({
  capability,
  node,
  surface,
  done,
  canAct,
  onDone,
}: {
  capability: string;
  node: string;
  surface: ButlerSurface;
  done: boolean;
  canAct: boolean;
  onDone: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const mint = () => {
    setBusy(true);
    setError(null);
    api
      .mintChatAdapter(capability, node, surface)
      .then(onDone)
      .catch((e: Error) => setError(e.message))
      .finally(() => setBusy(false));
  };
  return (
    <Step
      n={2}
      title="Give the adapter its Keystone credential"
      done={done}
      note={
        done
          ? `Minted. The token is on ${node} as secret/${capability}-adapter and holds converse and nothing else: it can relay messages, and cannot read the house.`
          : `Mints a ${surface === "admin" ? "valet" : "concierge"} principal and places its token on ${node}. Minting again later is rotation.`
      }
    >
      <div className="row">
        <button className="btn btn-secondary" disabled={busy || !canAct} onClick={mint}>
          {busy ? "Minting…" : done ? "Rotate" : "Mint credential"}
        </button>
      </div>
      <ErrorLine error={error} />
    </Step>
  );
}

function TokenStep({
  node,
  secret,
  done,
  canAct,
  onDone,
}: {
  node: string;
  secret: string;
  done: boolean;
  canAct: boolean;
  onDone: () => void;
}) {
  const [value, setValue] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const save = () => {
    setBusy(true);
    setError(null);
    api
      .setSecret(node, secret, value)
      .then(() => {
        setValue("");
        onDone();
      })
      .catch((e: Error) => setError(e.message))
      .finally(() => setBusy(false));
  };
  return (
    <Step
      n={3}
      title="Paste the bot token"
      done={done}
      note={
        secret === ""
          ? "This package declares no operator-supplied secret, so there is nothing to paste."
          : done
            ? `Set as secret/${secret}. It is write-only — nothing, this screen included, can read it back. Paste a new one to replace it.`
            : `The token the chat vendor gave you. It is stored as secret/${secret} on ${node}, the name this package reads, and is never readable again.`
      }
    >
      <div className="row">
        <input
          type="password"
          placeholder="123456:ABC-DEF…"
          value={value}
          onChange={(e) => setValue(e.target.value)}
        />
        <button
          className="btn btn-secondary"
          disabled={busy || value === "" || secret === "" || !canAct}
          onClick={save}
        >
          {busy ? "Saving…" : done ? "Replace" : "Save token"}
        </button>
      </div>
      <ErrorLine error={error} />
    </Step>
  );
}

export function InstallStep({
  pkg,
  node,
  nodes,
  done,
  onDone,
}: {
  pkg: PackageView;
  node: string;
  nodes: NodeInfo[];
  done: boolean;
  onDone: () => void;
}) {
  // Every param the package asks for, minus the node (chosen above).
  // Generated rather than hard-coded: chat-matrix-* declares a
  // homeserver_url that chat-telegram-* does not, and a form that sent
  // only Telegram's three params had every Matrix install rejected for a
  // missing required param before a plan could exist.
  const fields = (pkg.params ?? []).filter((p) => p.name !== "node");
  const [values, setValues] = useState<Record<string, string>>({});
  const [plan, setPlan] = useState<PlanRecord | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const valueOf = (name: string) => values[name] ?? "";
  const missing = fields
    .filter((f) => f.required && valueOf(f.name).trim() === "" && !f.suggested && !f.default)
    .map((f) => f.name);

  const propose = () => {
    setBusy(true);
    setError(null);
    const params: Record<string, string> = { node };
    for (const f of fields) {
      const v = valueOf(f.name).trim() || f.suggested || "";
      if (v !== "") params[f.name] = v;
    }
    api
      .enableCapability(pkg.capability, params)
      .then(setPlan)
      .catch((e: Error) => setError(e.message))
      .finally(() => setBusy(false));
  };

  const approve = () => {
    if (!plan) return;
    setBusy(true);
    api
      .approvePlan(plan.id)
      .then(() => {
        setPlan(null);
        onDone();
      })
      .catch((e: Error) => setError(e.message))
      .finally(() => setBusy(false));
  };

  return (
    <Step
      n={4}
      title="Install the adapter"
      done={done}
      note={
        done
          ? "Installed. The container dials out to the chat vendor and to Keystone: it opens no port and adds nothing at your edge."
          : `The image must be digest-pinned, and the NODE must be able to pull it — a public repository needs nothing further, a private one needs its credential on the node first, as a secret named after the registry (e.g. registry.ghcr.io). The Keystone URL is the one the CONTAINER sees, usually the tailnet address of ${nodes[0]?.id ?? "the node"} rather than 127.0.0.1.`
      }
    >
      {!done && (
        <>
          {fields.map((f) => (
            <div className="row" key={f.name}>
              <label style={{ minWidth: 140 }}>
                {f.name}
                {f.required ? " *" : ""}
              </label>
              <input
                type="text"
                style={{ flex: 1 }}
                placeholder={f.suggested || f.default || f.example || f.description || f.name}
                value={valueOf(f.name)}
                onChange={(e) => setValues({ ...values, [f.name]: e.target.value })}
              />
            </div>
          ))}
          <div className="row">
            <button className="btn btn-secondary" disabled={busy || missing.length > 0} onClick={propose}>
              {busy ? "Proposing…" : "Propose install"}
            </button>
            {missing.length > 0 && (
              <span className="muted">fill in {missing.join(", ")}</span>
            )}
          </div>
          {plan && (
            <div className="row">
              <span className="muted">
                Plan {plan.id} is waiting for you. Nothing has changed yet.
              </span>
              <button className="btn btn-primary" disabled={busy} onClick={approve}>
                Approve it
              </button>
            </div>
          )}
        </>
      )}
      <ErrorLine error={error} />
    </Step>
  );
}

// InviteStep is the payoff: pick the person, get a link that opens the
// bot with the code already in it.
export function InviteStep({
  surface,
  transport,
  members,
  principals,
}: {
  surface: ButlerSurface;
  transport?: ChatTransport;
  members: HouseholdMember[];
  principals: Principal[];
}) {
  const [identity, setIdentity] = useState("");
  const [claim, setClaim] = useState<ChatClaim | null>(null);
  const [copied, setCopied] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const options =
    surface === "family"
      ? members.map((m) => ({ value: m.username, label: m.displayName || m.username }))
      : principals.filter((p) => !p.disabled).map((p) => ({ value: p.name, label: p.name }));
  const who = identity || options[0]?.value || "";

  const invite = () => {
    setBusy(true);
    setError(null);
    setCopied(false);
    api
      .issueChatClaim(surface, who, transport?.transport)
      .then(setClaim)
      .catch((e: Error) => setError(e.message))
      .finally(() => setBusy(false));
  };

  const copy = () => {
    const text = claim?.link || claim?.code || "";
    navigator.clipboard?.writeText(text).then(
      () => setCopied(true),
      () => setCopied(false),
    );
  };

  return (
    <Step
      n={6}
      title="Invite someone"
      done={false}
      note={
        options.length === 0
          ? surface === "family"
            ? "Nobody has an account yet. Add someone on the Household screen first — an invite binds a chat account to a declared person."
            : "No principal to bind."
          : "The code works once, expires in minutes, and can only ever create the binding it names."
      }
    >
      <div className="row">
        <select
          value={who}
          disabled={options.length === 0}
          onChange={(e) => setIdentity(e.target.value)}
        >
          {options.length === 0 && <option value="">nobody to invite</option>}
          {options.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
        <button
          className="btn btn-primary"
          disabled={busy || who === "" || !transport?.installed}
          onClick={invite}
        >
          {busy ? "Issuing…" : "Get invite link"}
        </button>
      </div>
      <ErrorLine error={error} />
      {claim && (
        <div style={{ marginTop: 10 }}>
          <pre>{claim.link || claim.code}</pre>
          <div className="row">
            <button className="btn btn-secondary" onClick={copy}>
              {copied ? "Copied" : claim.link ? "Copy link" : "Copy code"}
            </button>
          </div>
          <div className="muted">{claim.note}</div>
          {!claim.link && (
            <div className="muted">
              No link yet: the bot has not checked in, so nothing here knows which bot to open. Read
              the code out instead — it works either way.
            </div>
          )}
          <div className="muted">
            This is the only time it is shown. If it is lost, issue another one.
          </div>
        </div>
      )}
    </Step>
  );
}
