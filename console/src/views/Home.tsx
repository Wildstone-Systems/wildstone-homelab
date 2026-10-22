import { ReactNode, useState } from "react";
import {
  ArrowSquareOut,
  CheckCircle,
  EnvelopeSimple,
  Plugs,
  SealCheck,
  Sparkle,
  WarningCircle,
} from "@phosphor-icons/react";
import { api, Observed, PackageView, PlanRecord, ResourceView } from "../api";
import { ago, ErrorLine, useLoad, when } from "../lib";
import { ConsoleSnapshot, useConsoleState } from "../state";
import { groupServices, ownerLabel, Service, ServiceGroup } from "../services";
import { Kind, kindOf, StatusPill } from "../ui/status";
import { Metric } from "../ui/Metric";
import { HourChart } from "../ui/HourChart";
import { ProblemCard } from "../ui/ProblemCard";
import { ServiceTile } from "../ui/ServiceTile";
import { Drawer } from "../ui/Drawer";
import { Sparkline } from "../ui/Sparkline";

export type Destination = "plans" | "butler" | "nodes" | "backups" | "activity" | "mail";

// Home (ADR-W027): three bands. What is true right now, what needs a
// decision, and what the household runs.
//
// The good day is the one that matters most and the one the old screen
// handled least: no banners, green metrics, just tiles.
export function Home({ onNavigate }: { onNavigate: (to: Destination) => void }) {
  const { snap } = useConsoleState();
  const catalog = useLoad(() => Promise.all([api.packages(), api.resources()]), []);

  if (catalog.error) return <ErrorLine error={catalog.error} />;
  if (!snap || !catalog.data) return <HomeSkeleton />;
  const [pkgs, resources] = catalog.data;
  return <HomeView snap={snap} pkgs={pkgs} resources={resources} onNavigate={onNavigate} />;
}

// The screen itself, separated from where its data comes from so the
// render check can put it in a state — the good day, the bad one, the
// empty statefile — rather than only ever seeing the skeleton.
export function HomeView({
  snap,
  pkgs,
  resources,
  onNavigate,
}: {
  snap: ConsoleSnapshot;
  pkgs: PackageView[];
  resources: ResourceView[];
  onNavigate: (to: Destination) => void;
}) {
  const [open, setOpen] = useState<Service | null>(null);
  const groups = groupServices(pkgs, resources, snap.observed);
  const problems = buildProblems(snap);
  const connected = snap.nodes.filter((n) => n.connected).length;

  return (
    <>
      <div className="strip">
        <Metric
          label="Resources"
          value={snap.unhealthy.length}
          unit={`unhealthy of ${resources.length}`}
          kind={snap.unhealthy.length ? "bad" : "ok"}
        >
          {snap.awaiting.length > 0 && (
            <StatusPill kind="warn">{snap.awaiting.length} waiting on a plan</StatusPill>
          )}
        </Metric>
        <Metric
          label="Waiting on you"
          value={snap.pending.length}
          unit={snap.pending.length === 1 ? "plan" : "plans"}
          kind={snap.pending.length ? "warn" : "ok"}
        >
          {snap.pending.slice(0, 3).map((p) => (
            <StatusPill key={p.id} kind={kindOf(p.risk)}>
              {p.risk}
            </StatusPill>
          ))}
        </Metric>
        <Metric
          label="Nodes"
          value={connected}
          unit={`of ${snap.nodes.length} connected`}
          kind={connected < snap.nodes.length ? "bad" : "ok"}
        >
          {snap.nodes
            .filter((n) => !n.connected)
            .slice(0, 3)
            .map((n) => (
              <StatusPill key={n.id} kind="bad">
                {n.id}
              </StatusPill>
            ))}
        </Metric>
        <HourChart events={snap.events} now={snap.lastPollAt} />
      </div>

      <div className="bands">
        {problems.length === 0 ? (
          <div className="allwell">
            <CheckCircle size={17} weight="fill" color="var(--st-ok)" />
            Everything converged. Nothing is waiting on you — last checked {ago(snap.lastPollAt)}.
          </div>
        ) : (
          problems.map((p, i) => (
            <ProblemRow key={p.id} problem={p} compact={i > 0} onNavigate={onNavigate} />
          ))
        )}

        {groups.map((g) => (
          <Group key={g.owner} group={g} snap={snap} onOpen={setOpen} />
        ))}

        {groups.length === 0 && (
          <div className="muted">Statefile is empty — enable a capability from Services.</div>
        )}

        {/* Your own mailbox monitor, when email is installed (epic
            #326): the way in from the screen you land on. */}
        {pkgs.some((p) => p.capability === "mail" && p.enabled) && (
          <div className="allwell">
            <EnvelopeSimple size={17} />
            Let an Agent watch your mailbox for events, with your approval.{" "}
            <button className="btn btn-ghost" onClick={() => onNavigate("mail")}>
              Watch this mailbox
            </button>
          </div>
        )}
      </div>

      <Drawer open={open !== null} onClose={() => setOpen(null)} title={open?.name}>
        {open && (
          <ServiceDetail
            service={open}
            group={groups.find((g) => g.services.some((s) => s.key === open.key))}
            samples={snap.history.get(open.key) ?? []}
            resources={resources}
          />
        )}
      </Drawer>
    </>
  );
}

function Group({
  group,
  snap,
  onOpen,
}: {
  group: ServiceGroup;
  snap: ConsoleSnapshot;
  onOpen: (s: Service) => void;
}) {
  const [closed, setClosed] = useState(false);
  return (
    <section>
      <div className="sect">
        <button
          type="button"
          className="btn btn-ghost group-name"
          onClick={() => setClosed((c) => !c)}
          aria-expanded={!closed}
        >
          {group.label}
        </button>
        <span className="muted">
          {group.services.length} {group.services.length === 1 ? "service" : "services"} ·{" "}
          {group.resourceCount} {group.resourceCount === 1 ? "resource" : "resources"}
          {group.version ? ` · ${group.version}` : ""}
        </span>
        <span className="fade" />
        {group.stale && (
          <StatusPill kind="warn" title={`catalog has ${group.catalogVersion}`}>
            update available
          </StatusPill>
        )}
      </div>
      {group.blockers.map((b) => (
        <div className="blocker" key={b}>
          <strong>Will not work here.</strong> {b}
        </div>
      ))}
      {!closed && (
        <div className="tiles">
          {group.services.map((s) => (
            <ServiceTile
              key={s.key}
              service={s}
              samples={snap.history.get(s.key) ?? []}
              onOpen={() => onOpen(s)}
            />
          ))}
          {group.services.length === 0 && (
            <div className="muted">No containers — this owner holds routes and config only.</div>
          )}
        </div>
      )}
    </section>
  );
}

function ServiceDetail({
  service,
  group,
  samples,
  resources,
}: {
  service: Service;
  group?: ServiceGroup;
  samples: Kind[];
  resources: ResourceView[];
}) {
  const owned = group ? resources.filter((r) => r.owner === group.owner) : [];
  return (
    <>
      <div className="eyebrow">{group ? ownerLabel(group.owner) : "service"}</div>
      <h3 style={{ fontSize: 22, margin: "var(--space-2) 0 var(--space-3)" }}>{service.name}</h3>
      <div className="row" style={{ marginTop: 0 }}>
        <StatusPill kind={service.health}>{service.state}</StatusPill>
        {service.node && <span className="muted">on {service.node}</span>}
      </div>
      {service.detail && <p className="problem-body">{service.detail}</p>}

      {service.url && (
        <div className="row">
          <a className="btn btn-primary" href={service.url} target="_blank" rel="noreferrer">
            Open {service.name}
            <ArrowSquareOut size={14} />
          </a>
          {service.urlHealthy === false && <StatusPill kind="bad">not answering</StatusPill>}
        </div>
      )}

      <div className="sect" style={{ marginTop: "var(--space-6)" }}>
        <span className="eyebrow">Recent checks</span>
        <span className="fade" />
      </div>
      <Sparkline samples={samples} height={22} />
      {samples.length < 4 && (
        <div className="muted">
          Not enough history yet — the console has been open {samples.length} check
          {samples.length === 1 ? "" : "s"}.
        </div>
      )}

      <div className="sect" style={{ marginTop: "var(--space-6)" }}>
        <span className="eyebrow">What it is made of</span>
        <span className="fade" />
      </div>
      <table>
        <thead>
          <tr>
            <th>kind</th>
            <th>name</th>
            <th>node</th>
          </tr>
        </thead>
        <tbody>
          {owned.map((r) => (
            <tr key={`${r.kind}/${r.name}`}>
              <td>{r.kind}</td>
              <td>{r.name}</td>
              <td>{r.node ?? "—"}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  );
}

// ------------------------------------------------------------ problems

interface Problem {
  id: string;
  severity: Kind;
  icon: ReactNode;
  title: string;
  body: string;
  since?: string;
  plan?: PlanRecord;
  to: Destination;
  cta: string;
}

// One card per thing that needs a decision, worst first. A pending plan
// that already explains an unhealthy resource is folded into that
// resource's card rather than repeated as its own — the operator has
// one problem, not two.
export function buildProblems(snap: ConsoleSnapshot): Problem[] {
  const out: Problem[] = [];
  const claimed = new Set<string>();

  for (const o of snap.unhealthy) {
    const plan = snap.pending.find((p) => mentions(p, o));
    if (plan) claimed.add(plan.id);
    out.push({
      id: `observed:${o.kind}/${o.name}`,
      severity: "bad",
      icon: <WarningCircle size={16} weight="fill" color="var(--st-bad)" />,
      title: `${o.name} is unhealthy`,
      body: o.detail ?? `${o.kind}/${o.name} is not converged and the reconciler has not said why.`,
      since: o.updatedAt,
      plan,
      to: plan ? "plans" : "butler",
      cta: plan ? "Review plan" : "Ask why",
    });
  }

  for (const n of snap.nodes.filter((n) => !n.connected)) {
    out.push({
      id: `node:${n.id}`,
      severity: "bad",
      icon: <Plugs size={16} weight="fill" color="var(--st-bad)" />,
      title: `${n.id} is not connected`,
      body: `The gateway has no live session for ${n.id}. Nothing on it reconciles until it comes back — resources there are stale, not broken.`,
      since: n.lastSeenAt,
      to: "nodes",
      cta: "Open Nodes",
    });
  }

  for (const p of snap.pending) {
    if (claimed.has(p.id)) continue;
    out.push({
      id: `plan:${p.id}`,
      severity: "warn",
      icon: <SealCheck size={16} weight="fill" color="var(--st-warn)" />,
      title: p.summary,
      body: `Proposed by ${p.principal}${p.actingFor ? ` for ${p.actingFor}` : ""}. Approving records who approved it and hands it to the reconciler.`,
      since: p.createdAt,
      plan: p,
      to: "plans",
      cta: "Review plan",
    });
  }

  return out;
}

// A plan is about a resource if any of its steps names it. Plans that
// carry no expanded steps fall back to their summary, which is how the
// Butler's proposals read before they are opened.
function mentions(p: PlanRecord, o: Observed): boolean {
  const steps = p.plan?.steps ?? [];
  if (steps.some((s) => s.kind === o.kind && s.name === o.name)) return true;
  return steps.length === 0 && p.summary.includes(o.name);
}

function ProblemRow({
  problem,
  compact,
  onNavigate,
}: {
  problem: Problem;
  compact: boolean;
  onNavigate: (to: Destination) => void;
}) {
  const meta = (
    <>
      {problem.plan && (
        <StatusPill kind={kindOf(problem.plan.risk)}>
          {problem.plan.id} · {problem.plan.risk}
        </StatusPill>
      )}
      {problem.since && (
        <span className="muted" title={when(problem.since)}>
          since {ago(problem.since)}
        </span>
      )}
    </>
  );

  return (
    <ProblemCard
      severity={problem.severity}
      icon={problem.icon}
      title={compact ? <span style={{ fontSize: 14 }}>{problem.title}</span> : problem.title}
      meta={meta}
      body={compact ? undefined : problem.body}
      compact={compact}
      actions={
        <>
          <button className="btn btn-primary" onClick={() => onNavigate(problem.to)}>
            {problem.to === "butler" ? <Sparkle size={14} /> : <SealCheck size={14} />}
            {problem.cta}
          </button>
          {!compact && problem.to !== "butler" && (
            <button className="btn btn-secondary" onClick={() => onNavigate("butler")}>
              <Sparkle size={14} />
              Ask why
            </button>
          )}
        </>
      }
    />
  );
}

function HomeSkeleton() {
  return <div className="muted">Loading the household…</div>;
}
