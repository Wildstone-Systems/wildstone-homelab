import { ReactNode, useEffect, useRef } from "react";
import { X } from "@phosphor-icons/react";

// A right-hand detail panel. It never navigates away, so the list
// behind it keeps its scroll position and the operator keeps their
// place — the whole reason detail is a drawer and not a tenth screen.
export function Drawer({
  open,
  onClose,
  title,
  children,
}: {
  open: boolean;
  onClose: () => void;
  title?: string;
  children: ReactNode;
}) {
  const panel = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    document.addEventListener("keydown", onKey);
    panel.current?.focus();
    return () => document.removeEventListener("keydown", onKey);
  }, [open, onClose]);

  if (!open) return null;
  return (
    <div className="drawer-scrim" onClick={onClose}>
      <div
        className="drawer"
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
