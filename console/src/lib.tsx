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

export function Badge({ value }: { value: string }) {
  const cls = value.toLowerCase().replace(/[^a-z]/g, "");
  const known = [
    "low",
    "medium",
    "high",
    "applied",
    "proposed",
    "failed",
    "rejected",
    "succeeded",
    "running",
    "ok",
    "bad",
  ];
  return <span className={`badge ${known.includes(cls) ? cls : "neutral"}`}>{value}</span>;
}

export function ErrorLine({ error }: { error: string | null }) {
  return error ? <div className="error">{error}</div> : null;
}

export function when(iso?: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  return isNaN(d.getTime()) ? iso : d.toLocaleString();
}
