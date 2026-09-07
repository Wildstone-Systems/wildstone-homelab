import { ReactNode, useEffect, useRef } from "react";
import { X } from "@phosphor-icons/react";

// A right-hand detail panel. It never navigates away, so the list
// behind it keeps its scroll position and the operator keeps their
// place — the whole reason detail is a drawer and not a tenth screen.
export function Drawer({
  open,
  onClose,
  title,
  wide,
  children,
}: {
  open: boolean;
  onClose: () => void;
  title?: string;
  // A form needs more room than a detail panel: the install form has
  // labelled fields and a preview, and 440px turns every label onto
  // its own line.
  wide?: boolean;
  children: ReactNode;
}) {
  const panel = useRef<HTMLDivElement>(null);
  // Callers pass an inline arrow, so onClose is a new function every
  // render. Depending on it would re-run this effect each time and
  // re-focus the panel out from under whatever the reader had focused.
  const close = useRef(onClose);
  close.current = onClose;

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") close.current();
    };
    document.addEventListener("keydown", onKey);
    panel.current?.focus();
    return () => document.removeEventListener("keydown", onKey);
  }, [open]);

  if (!open) return null;
  return (
    <div className="drawer-scrim" onClick={onClose}>
      <div
        className={wide ? "drawer drawer-wide" : "drawer"}
        role="dialog"
        aria-modal="true"
        aria-label={title}
        tabIndex={-1}
        ref={panel}
        onClick={(e) => e.stopPropagation()}
      >
        <button type="button" className="btn btn-ghost drawer-close" onClick={onClose} aria-label="Close">
          <X size={16} />
        </button>
        {children}
      </div>
    </div>
  );
}
