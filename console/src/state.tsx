import { createContext, ReactNode, useContext, useEffect, useRef, useState } from "react";
import { api, EventRow, NodeInfo, Observed, PlanRecord } from "./api";
import { Kind } from "./ui/status";

const POLL_MS = 4000;
const HISTORY = 14;
const KEEP = 300;

export interface ConsoleSnapshot {
  observed: Observed[];
  nodes: NodeInfo[];
  // Newest last, the order the API serves and the order the cursor
  // advances in. Screens that want newest first reverse a copy.
  events: EventRow[];
  plans: PlanRecord[];
  pending: PlanRecord[];
  unhealthy: Observed[];
  awaiting: Observed[];
  history: Map<string, Kind[]>;
  lastPollAt: Date;
}

interface ConsoleState {
  snap: ConsoleSnapshot | null;
  error: string | null;
}

const Ctx = createContext<ConsoleState>({ snap: null, error: null });

// One poll for the whole shell. The rail pips, the live pill, Home's
// metric strip and Activity all read from this — one timer and one
// context, not five useLoads racing each other and the rate limiter.
export function ConsoleStateProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<ConsoleState>({ snap: null, error: null });
  const cursor = useRef(0);
  const history = useRef(new Map<string, Kind[]>());
  const events = useRef<EventRow[]>([]);

  useEffect(() => {
    let stop = false;
    const tick = async () => {
      try {
        const [observed, plans, nodes, batch] = await Promise.all([
          api.observed(),
          api.plans(),
          api.nodes(),
          api.events(cursor.current),
        ]);
        if (stop) return;
        if (batch.length > 0) {
          cursor.current = batch[batch.length - 1].seq;
          events.current = [...events.current, ...batch].slice(-KEEP);
        }

        // Accumulate per-resource health for the sparklines. Three
        // states, not two (F75): a resource held back by a plan
        // awaiting a human is amber, and never the red of a fault.
        for (const o of observed) {
          const key = `${o.kind}/${o.name}`;
          const kind: Kind = o.healthy ? "ok" : o.awaitingApproval ? "warn" : "bad";
          const prev = history.current.get(key) ?? [];
          history.current.set(key, [...prev, kind].slice(-HISTORY));
        }

        setState({
          error: null,
          snap: {
            observed,
            plans,
            nodes: nodes.nodes,
            events: events.current,
            pending: plans.filter((p) => p.state === "proposed"),
            unhealthy: observed.filter((o) => !o.healthy && !o.awaitingApproval),
            awaiting: observed.filter((o) => !o.healthy && o.awaitingApproval === true),
            history: new Map(history.current),
            lastPollAt: new Date(),
          },
        });
      } catch (e) {
        // Keep the last good snapshot on screen. A dropped poll is not
        // a reason to blank the console; the header says it is stale.
        if (!stop) setState((s) => ({ ...s, error: (e as Error).message }));
      }
    };
    void tick();
    const id = setInterval(tick, POLL_MS);
    return () => {
      stop = true;
      clearInterval(id);
    };
  }, []);

  return <Ctx.Provider value={state}>{children}</Ctx.Provider>;
}

export function useConsoleState(): ConsoleState {
  return useContext(Ctx);
}

// The sparkline samples for one resource, or none if the console has
// not been open long enough to have any.
export function samplesFor(snap: ConsoleSnapshot | null, kind: string, name: string): Kind[] {
  return snap?.history.get(`${kind}/${name}`) ?? [];
}

export { POLL_MS, KEEP };
