import { ReactNode } from "react";
import { Kind, KIND_COLOR } from "./status";

export interface RailItem<T extends string> {
  id: T;
  label: string;
  icon: ReactNode;
  // A pip means "this screen has something for you". No pip is the
  // resting state, and the healthy console has none.
  pip?: Kind;
  // Draws a hairline above this item: the infrastructure screens sit
  // below it, the daily ones above.
  dividerBefore?: boolean;
}

// Icons carry the nav, so every item keeps its label as both a tooltip
// and an accessible name. On a phone the rail becomes a bottom bar —
// that is CSS, not a second component.
export function Rail<T extends string>({
  items,
  current,
  onSelect,
  mark,
  footer,
}: {
  items: RailItem<T>[];
  current: T;
  onSelect: (id: T) => void;
  mark?: ReactNode;
  footer?: ReactNode;
}) {
  return (
    <nav className="rail" aria-label="Console sections">
      {mark && <div className="mark">{mark}</div>}
      {items.map((it) => (
        <div key={it.id} style={{ display: "contents" }}>
          {it.dividerBefore && <span className="divider" aria-hidden />}
          <button
            type="button"
            className={current === it.id ? "ri on" : "ri"}
            title={it.label}
            aria-label={it.label}
            aria-current={current === it.id ? "page" : undefined}
            onClick={() => onSelect(it.id)}
          >
            {it.icon}
            {it.pip && (
              <span className="pip" style={{ background: KIND_COLOR[it.pip] }} aria-hidden />
            )}
          </button>
        </div>
      ))}
      {footer && <div className="foot">{footer}</div>}
    </nav>
  );
}
