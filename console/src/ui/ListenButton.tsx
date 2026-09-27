import { CircleNotch, SpeakerHigh, Stop } from "@phosphor-icons/react";
import { useEffect, useRef, useState } from "react";
import { api } from "../api";

// The listen button under a Butler's reply (ADR-W042 part two). It asks
// Keystone to read the reply with the voice the Butler's privacy class
// allows, and plays what comes back. The dashboard has the same button
// in plain script (keystone/portal/web); this is its Console twin.
//
// One reply plays at a time on the page: starting another stops this
// one, and leaving the page stops it.

type State = "idle" | "working" | "playing";

// current is whichever button is playing or fetching, page-wide.
let current: { stop: () => void } | null = null;

export function ListenButton({
  butler,
  text,
  autoPlay,
  onStarted,
}: {
  butler: string;
  text: string;
  // Read at once: the answer to a question the person dictated.
  autoPlay?: boolean;
  onStarted?: () => void;
}) {
  const [state, setState] = useState<State>("idle");
  const [note, setNote] = useState<{ message: string; bad?: boolean }>({ message: "" });
  // ticket changes on every start and stop, so audio that arrives after
  // the person stopped it, or started another, is dropped.
  const ticket = useRef(0);
  const audio = useRef<HTMLAudioElement | null>(null);
  const url = useRef("");
  const mounted = useRef(true);
  const me = useRef({ stop: () => {} });

  const stop = () => {
    ticket.current++;
    audio.current?.pause();
    audio.current = null;
    if (url.current) URL.revokeObjectURL(url.current);
    url.current = "";
    if (current === me.current) current = null;
    if (mounted.current) setState("idle");
  };
  me.current.stop = stop;

  const play = () => {
    current?.stop();
    current = me.current;
    const mine = ++ticket.current;
    setNote({ message: "" });
    setState("working");
    onStarted?.();
    api
      .speakForButler(butler, text)
      .then((blob) => {
        if (mine !== ticket.current || !mounted.current) return;
        url.current = URL.createObjectURL(blob);
        const a = new Audio(url.current);
        audio.current = a;
        a.onended = () => {
          if (mine === ticket.current) stop();
        };
        // "playing" only once sound has started: a browser may refuse to
        // start it at all.
        return a.play().then(() => {
          if (mine === ticket.current && mounted.current) setState("playing");
        });
      })
      .catch((err: unknown) => {
        if (mine !== ticket.current || !mounted.current) return;
        stop();
        // A browser that will not start sound without a tap says so; the
        // button is there to tap.
        const e = err as { name?: string; message?: string };
        setNote(
          e?.name === "NotAllowedError"
            ? { message: "Press the speaker to hear the reply." }
            : { message: String(e?.message || err), bad: true },
        );
      });
  };

  useEffect(() => {
    mounted.current = true;
    if (autoPlay) play();
    return () => {
      mounted.current = false;
      stop();
    };
    // Mount only: autoPlay reads the reply once, when it first appears.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const busy = state !== "idle";
  return (
    <div className="listen-row">
      <button
        type="button"
        className="btn btn-ghost listen"
        onClick={() => (busy ? stop() : play())}
        aria-pressed={busy}
        aria-label={busy ? "Stop reading" : "Listen"}
        title={busy ? "Stop reading" : "Read this reply aloud"}
        data-state={state}
      >
        {state === "playing" ? (
          <Stop size={14} weight="fill" />
        ) : state === "working" ? (
          <CircleNotch size={14} />
        ) : (
          <SpeakerHigh size={14} />
        )}
      </button>
      {note.message && (
        <span className={"mic-note" + (note.bad ? " mic-note-bad" : "")} aria-live="polite">
          {note.message}
        </span>
      )}
    </div>
  );
}
