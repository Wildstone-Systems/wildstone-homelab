import { useCallback, useEffect, useState } from "react";

// useLoad: the Console's one data-fetch pattern — load on mount, expose
// reload, surface the error inline. Boring on purpose.
export function useLoad<T>(fn: () => Promise<T>, deps: unknown[] = []) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const reload = useCallback(() => {
    setLoading(true);
    fn()
      .then((d) => {
        setData(d);
        setError(null);
      })
      .catch((e: Error) => setError(e.message))
      .finally(() => setLoading(false));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
  useEffect(reload, [reload]);
  return { data, error, loading, reload };
}

export type ActionState =
  | { phase: "idle" }
  | { phase: "busy" }
  | { phase: "done"; message: string }
  | { phase: "error"; message: string };

// useAction: the counterpart to useLoad for the write half. It replaces
// the busy/actionError pair that was copied into Plans, Backups,
// Secrets, Nodes and Services, and it keeps the RESULT in the state —
// so the card that fired the action can say what happened instead of
// silently reloading and looking untouched.
export function useAction() {
  const [state, setState] = useState<ActionState>({ phase: "idle" });

  const run = useCallback(async (fn: () => Promise<unknown>, doneMessage: string) => {
    setState({ phase: "busy" });
    try {
      await fn();
      setState({ phase: "done", message: doneMessage });
    } catch (e) {
      setState({ phase: "error", message: (e as Error).message });
    }
  }, []);

  const reset = useCallback(() => setState({ phase: "idle" }), []);

  return { state, run, reset };
}

export function ErrorLine({ error }: { error: string | null }) {
  return error ? <div className="error">{error}</div> : null;
}

export function when(iso?: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  return isNaN(d.getTime()) ? iso : d.toLocaleString();
}

// "3d 4h" — how long something has been the way it is. Uptime reads as
// a duration; a join date reads as a date, and they are not the same
// question.
export function duration(since?: string, now: Date = new Date()): string {
  if (!since) return "—";
  const d = new Date(since);
  if (isNaN(d.getTime())) return "—";
  const secs = Math.max(0, Math.round((now.getTime() - d.getTime()) / 1000));
  const days = Math.floor(secs / 86400);
  const hours = Math.floor((secs % 86400) / 3600);
  const mins = Math.floor((secs % 3600) / 60);
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${mins}m`;
  if (mins > 0) return `${mins}m`;
  return `${secs}s`;
}

// "in 14m" — how long is left. ago() clamps at zero, so it reads a
// future timestamp as "0s ago"; an expiry needs the other direction.
export function until(iso?: string, now: Date = new Date()): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  const secs = Math.round((d.getTime() - now.getTime()) / 1000);
  if (secs <= 0) return "expired";
  if (secs < 60) return `in ${secs}s`;
  if (secs < 3600) return `in ${Math.round(secs / 60)}m`;
  if (secs < 86400) return `in ${Math.round(secs / 3600)}h`;
  return `in ${Math.round(secs / 86400)}d`;
}

// "12s ago" / "4m ago" — the header says how fresh the console is, and
// an operator reads an age faster than a timestamp.
export function ago(at?: Date | string, now: Date = new Date()): string {
  if (!at) return "never";
  const d = typeof at === "string" ? new Date(at) : at;
  if (isNaN(d.getTime())) return "never";
  const secs = Math.max(0, Math.round((now.getTime() - d.getTime()) / 1000));
  if (secs < 60) return `${secs}s ago`;
  if (secs < 3600) return `${Math.round(secs / 60)}m ago`;
  if (secs < 86400) return `${Math.round(secs / 3600)}h ago`;
  return `${Math.round(secs / 86400)}d ago`;
}

// Dismissals, kept in this browser.
//
// A reconcile failure is an event, not a row with a read flag — the
// console reads it out of the poll's kept window and the API has
// nowhere to record "I have seen this". So the dismissal lives here,
// keyed by plan id, and is honest about its scope: it clears the card
// on this browser and nowhere else. The plan itself stays in the
// resolved history and on the Activity stream either way, which is what
// makes dismissing safe rather than destructive.
const DISMISS_KEY = "wildstone.plans.dismissedFailures";

export function useDismissals(storageKey: string = DISMISS_KEY) {
  const [ids, setIds] = useState<string[]>(() => read(storageKey));

  const write = useCallback(
    (next: string[]) => {
      setIds(next);
      try {
        // Only ever our own keys, and only the newest 200 of them: a
        // household that never clears one should not grow a key without
        // a ceiling.
        localStorage.setItem(storageKey, JSON.stringify(next.slice(-200)));
      } catch {
        // Private mode, or storage disabled. The dismissal still holds
        // for this page — it just does not survive a reload.
      }
    },
    [storageKey],
  );

  return {
    ids,
    dismiss: (id: string) => write([...new Set([...ids, id])]),
    restoreAll: () => write([]),
  };
}

// Server-side rendering has no localStorage at all, and a browser can
// have one that throws or holds something that is not our array. All
// three read as "nothing dismissed".
function read(storageKey: string): string[] {
  try {
    const parsed: unknown = JSON.parse(localStorage.getItem(storageKey) ?? "[]");
    return Array.isArray(parsed) ? parsed.filter((v): v is string => typeof v === "string") : [];
  } catch {
    return [];
  }
}
