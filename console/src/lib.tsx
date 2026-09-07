import { useCallback, useEffect, useState } from "react";
import { kindOf, StatusPill } from "./ui/status";

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

// Badge: the pre-redesign call site for a status word. It now renders a
// StatusPill so the screens that have not been rebuilt yet already
// speak the new vocabulary; it goes when the last of them lands.
export function Badge({ value }: { value: string }) {
  return <StatusPill kind={kindOf(value)}>{value}</StatusPill>;
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
