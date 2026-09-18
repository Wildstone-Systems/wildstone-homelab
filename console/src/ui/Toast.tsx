import { useCallback, useEffect, useRef, useState } from "react";
import { Kind } from "./status";

export interface ToastMessage {
  text: string;
  kind: Kind;
}

// A bottom-centre pill that says what just happened.
//
// It exists because most of this screen applies immediately (ADR-W031
// A8): there is no dialog to carry the consequence, so something has to
// say "health now answers on qwen2.5:14b" in the moment it became true.
//
// It is polite rather than assertive, and it never carries an error.
// Anything a person has to act on stays on the screen as an ErrorLine —
// a message that disappears after 2.8 seconds is the wrong home for
// something that failed.
export function useToast() {
  const [toast, setToast] = useState<ToastMessage | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const say = useCallback((text: string, kind: Kind = "ok") => {
    setToast({ text, kind });
    if (timer.current) clearTimeout(timer.current);
    timer.current = setTimeout(() => setToast(null), 2800);
  }, []);

  useEffect(() => () => void (timer.current && clearTimeout(timer.current)), []);
  return { toast, say };
}

export function Toast({ toast }: { toast: ToastMessage | null }) {
  if (!toast) return null;
  return (
    <div className="toast" role="status" aria-live="polite">
      <span className={`dot dot-${toast.kind}`} aria-hidden />
      {toast.text}
    </div>
  );
}
