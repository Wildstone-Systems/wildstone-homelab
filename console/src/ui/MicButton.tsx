import { Microphone, Stop } from "@phosphor-icons/react";
import { useEffect, useRef, useState } from "react";
import { api } from "../api";

// Dictation in the Console (ADR-W042): record in the browser, have
// Keystone write down what was said, and put the words in the text box.
// Nothing is sent to the Butler until the person presses Send, so a
// mis-heard word is corrected before it is a question.
//
// The household dashboard has the same control in plain script
// (keystone/portal/web); this is its Console twin and keeps the same
// rules: red while listening, stops itself just short of the limit, and
// a recording or a transcript still in flight is dropped when the
// Butler changes.

// starting is the wait for the browser's microphone permission, which
// can take as long as the person takes to answer the prompt.
type State = "idle" | "starting" | "recording" | "working";

// The first of these the browser can record: Chrome, Edge and Firefox
// take WebM/Opus, Safari takes MP4. Keystone accepts all of them.
const TYPES = ["audio/webm;codecs=opus", "audio/webm", "audio/ogg;codecs=opus", "audio/mp4"];

function pickType(): string {
  for (const t of TYPES) {
    if (typeof MediaRecorder.isTypeSupported === "function" && MediaRecorder.isTypeSupported(t)) return t;
  }
  return "";
}

// supported says whether this browser, on this page, can record at all.
// Without HTTPS or a recorder the button is never shown, rather than
// shown and failing when pressed.
export function dictationSupported(): boolean {
  return (
    typeof window !== "undefined" &&
    window.isSecureContext &&
    typeof MediaRecorder !== "undefined" &&
    !!navigator.mediaDevices?.getUserMedia
  );
}

function micError(err: unknown): string {
  const name = (err as { name?: string })?.name;
  if (name === "NotAllowedError" || name === "SecurityError") {
    return "The microphone is blocked. Allow it for this site in your browser to dictate, or type instead.";
  }
  if (name === "NotFoundError" || name === "OverconstrainedError") return "No microphone was found on this device.";
  return "The microphone couldn't start. You can still type.";
}

function limitPhrase(seconds: number): string {
  if (seconds % 60 !== 0) return `${seconds} seconds`;
  return seconds === 60 ? "1 minute" : `${seconds / 60} minutes`;
}

export function MicButton({
  butler,
  maxSeconds,
  disabled,
  onText,
  onNote,
}: {
  butler: string;
  maxSeconds: number;
  disabled?: boolean;
  // The words that came back, for the caller to add to its text box.
  onText: (text: string) => void;
  // A status line or an error (bad) for under the text box; "" clears.
  onNote: (message: string, bad?: boolean) => void;
}) {
  const [state, setState] = useState<State>("idle");
  const recorder = useRef<MediaRecorder | null>(null);
  const stream = useRef<MediaStream | null>(null);
  const chunks = useRef<Blob[]>([]);
  const started = useRef(0);
  const timer = useRef<number | undefined>(undefined);
  // generation changes with the Butler. A recording or a transcript
  // that belongs to an older generation is dropped: words meant for one
  // Butler are not typed into a conversation with another.
  const generation = useRef(0);
  const discard = useRef(false);
  const unmounted = useRef(false);
  // The caller's callbacks as of the latest render. A recording only
  // ever reports into the generation it started in, so the latest
  // callbacks are the right ones whenever a report is still wanted.
  const onTextRef = useRef(onText);
  const onNoteRef = useRef(onNote);
  onTextRef.current = onText;
  onNoteRef.current = onNote;

  const release = () => {
    stream.current?.getTracks().forEach((t) => t.stop());
    stream.current = null;
  };
  const stop = () => {
    window.clearTimeout(timer.current);
    if (recorder.current && recorder.current.state !== "inactive") recorder.current.stop();
  };

  useEffect(() => {
    generation.current++;
    if (recorder.current && recorder.current.state !== "inactive") {
      discard.current = true;
      stop();
    }
    onNoteRef.current("");
  }, [butler]);

  // Leaving the page never leaves the microphone on.
  useEffect(
    () => () => {
      unmounted.current = true;
      window.clearTimeout(timer.current);
      discard.current = true;
      if (recorder.current && recorder.current.state !== "inactive") recorder.current.stop();
      release();
    },
    [],
  );

  // finish sends a stopped recording to the Butler it was made for, and
  // types the words only if that Butler is still the one on screen.
  const finish = (mine: number, forButler: string) => {
    release();
    const rec = recorder.current;
    recorder.current = null;
    const blob = new Blob(chunks.current, { type: rec?.mimeType || "audio/webm" });
    chunks.current = [];
    if (discard.current || mine !== generation.current || unmounted.current) {
      discard.current = false;
      if (!unmounted.current) setState("idle");
      return;
    }
    if (!blob.size) {
      setState("idle");
      onNoteRef.current("Nothing was recorded. Try again.", true);
      return;
    }
    const seconds = (Date.now() - started.current) / 1000;
    setState("working");
    onNoteRef.current("Writing down what you said…");
    api
      .transcribeForButler(forButler, blob, seconds)
      .then((res) => {
        if (mine !== generation.current || unmounted.current) return;
        onTextRef.current(String(res.text || "").trim());
        onNoteRef.current("Check what I heard, then send.");
      })
      .catch((err: unknown) => {
        if (mine !== generation.current || unmounted.current) return;
        onNoteRef.current(String((err as Error)?.message || err), true);
      })
      .finally(() => {
        if (!unmounted.current) setState("idle");
      });
  };

  const start = async () => {
    // The Butler this recording is for is fixed HERE, before the
    // permission prompt: a switch while the prompt is open must not
    // send one Butler's audio to another, or to another's engine.
    const mine = generation.current;
    const forButler = butler;
    setState("starting");
    let s: MediaStream;
    try {
      s = await navigator.mediaDevices.getUserMedia({ audio: true });
    } catch (err) {
      if (unmounted.current) return;
      setState("idle");
      if (mine === generation.current) onNoteRef.current(micError(err), true);
      return;
    }
    if (unmounted.current || mine !== generation.current) {
      // The Butler changed, or the page went, while the prompt was open.
      s.getTracks().forEach((t) => t.stop());
      if (!unmounted.current) setState("idle");
      return;
    }
    try {
      stream.current = s;
      chunks.current = [];
      discard.current = false;
      const type = pickType();
      const rec = type ? new MediaRecorder(s, { mimeType: type }) : new MediaRecorder(s);
      rec.ondataavailable = (e) => {
        if (e.data && e.data.size) chunks.current.push(e.data);
      };
      rec.onstop = () => finish(mine, forButler);
      recorder.current = rec;
      rec.start();
      started.current = Date.now();
      setState("recording");
      const max = maxSeconds || 120;
      onNoteRef.current(`Listening… press the mic again when you're done (up to ${limitPhrase(max)}).`);
      // Just short of the server's limit, so a full-length recording is
      // never refused for a fraction of a second.
      timer.current = window.setTimeout(stop, Math.max(1, max - 1) * 1000);
    } catch (err) {
      release();
      setState("idle");
      onNoteRef.current(micError(err), true);
    }
  };

  const recording = state === "recording";
  return (
    <button
      type="button"
      className={"btn btn-secondary mic" + (recording ? " mic-recording" : "")}
      disabled={disabled || state === "working" || state === "starting"}
      onClick={() => (recording ? stop() : state === "idle" ? void start() : undefined)}
      aria-pressed={recording}
      aria-label={recording ? "Stop dictating" : "Dictate"}
      title={recording ? "Stop and write down what you said" : "Dictate instead of typing"}
      data-state={state}
    >
      {recording ? <Stop size={15} weight="fill" /> : <Microphone size={15} />}
    </button>
  );
}
