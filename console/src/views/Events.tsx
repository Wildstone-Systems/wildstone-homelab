import { useEffect, useRef, useState } from "react";
import { api, EventRow } from "../api";
import { ErrorLine, when } from "../lib";

const POLL_MS = 4000;
const KEEP = 300;

// Events (ADR-W016 A4): live via cursor polling of the existing
// ?after=<seq> endpoint — no new API surface for push at MVP.
export function Events() {
  const [rows, setRows] = useState<EventRow[]>([]);
  const [error, setError] = useState<string | null>(null);
  const cursor = useRef(0);

  useEffect(() => {
    let stop = false;
    const tick = async () => {
      try {
        const batch = await api.events(cursor.current);
        if (stop) return;
        if (batch.length > 0) {
          cursor.current = batch[batch.length - 1].seq;
          setRows((prev) => [...prev, ...batch].slice(-KEEP));
        }
        setError(null);
      } catch (e) {
        if (!stop) setError((e as Error).message);
      }
    };
    tick();
    const id = setInterval(tick, POLL_MS);
    return () => {
      stop = true;
      clearInterval(id);
    };
  }, []);

  return (
    <>
      <h2>Events</h2>
      <div className="sub">Operational stream, newest last; polling every {POLL_MS / 1000}s.</div>
      <ErrorLine error={error} />
      <table>
        <thead>
          <tr>
            <th>time</th>
            <th>type</th>
            <th>node</th>
            <th>message</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((e) => (
            <tr key={e.seq}>
              <td>{when(e.time)}</td>
              <td>{e.type}</td>
              <td>{e.node ?? ""}</td>
              <td>
                {e.message ?? ""}
                {e.attrs && Object.keys(e.attrs).length > 0 && (
                  <span className="muted">
                    {" "}
                    {Object.entries(e.attrs)
                      .map(([k, v]) => `${k}=${v}`)
                      .join(" ")}
                  </span>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {rows.length === 0 && <div className="muted">No events yet.</div>}
    </>
  );
}
