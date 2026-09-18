import { ReactNode } from "react";
import { Kind } from "./status";

// A confirmation that states a consequence rather than asking whether
// somebody is sure.
//
// The distinction is the whole reason this component exists. "Are you
// sure?" is a speed bump: it adds a click and tells the person nothing
// they did not already know, so they learn to click through it. What
// this asks instead is "here is what stops working — still?", with the
// list counted from the server. That is worth a dialog; the other is
// not, which is why only two acts on the Butlers screen get one.
//
// The cancel button carries the OLD state in its label ("Keep
// llama3.1:8b"), not the word Cancel. At the moment somebody is
// deciding, the useful thing to name is what they keep.
export function Confirm({
  kind,
  icon,
  title,
  body,
  listTitle,
  lines,
  option,
  confirmLabel,
  cancelLabel,
  busy,
  onConfirm,
  onCancel,
}: {
  kind: Kind;
  icon: ReactNode;
  title: string;
  body: string;
  listTitle?: string;
  lines?: string[];
  // An extra decision the confirmation can carry, when there is
  // something the person can do about the damage rather than only
  // accept it.
  option?: { label: string; checked: boolean; onToggle: () => void };
  confirmLabel: string;
  cancelLabel: string;
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  return (
    <div className="modal-scrim" onClick={onCancel}>
      <div
        className="modal modal-narrow"
        role="alertdialog"
        aria-modal="true"
        aria-label={title}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="modal-head">
          <span className={`modal-icon st-${kind}`} aria-hidden>
            {icon}
          </span>
          <h3>{title}</h3>
        </div>
        <div className="modal-body">
          <p>{body}</p>
          {lines && lines.length > 0 && (
            <div className={`consequence st-${kind}`}>
              <div className="eyebrow">{listTitle}</div>
              <ul>
                {lines.map((l) => (
                  <li key={l}>{l}</li>
                ))}
              </ul>
            </div>
          )}
          {option && (
            <label className="checkrow">
              <input type="checkbox" checked={option.checked} onChange={option.onToggle} />
              <span>{option.label}</span>
            </label>
          )}
        </div>
        <div className="modal-foot">
          <button
            type="button"
            className={`btn btn-${kind === "bad" ? "danger" : "warn"}`}
            disabled={busy}
            onClick={onConfirm}
          >
            {busy ? "Working…" : confirmLabel}
          </button>
          <button type="button" className="btn btn-secondary" disabled={busy} onClick={onCancel}>
            {cancelLabel}
          </button>
        </div>
      </div>
    </div>
  );
}
