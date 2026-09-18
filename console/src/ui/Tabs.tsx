// A tab strip for a drawer.
//
// It is here rather than inside the Butler drawer because the shape is
// not the Butler's: "one subject, several faces of it, without leaving
// the list behind" is what Work's detail wants too. Keeping it shared
// is also what stops a second screen growing a strip that looks nearly
// the same and behaves slightly differently.
//
// Buttons rather than links: the drawer does not navigate, so a tab
// that changed the URL would put a state the roster cannot restore
// into the back button.
export interface TabItem<T extends string> {
  id: T;
  label: string;
  // A count shown beside the label. Rendered only when it is truthy —
  // "Memory 0" reads as a broken counter, "Memory" reads as a tab.
  count?: string | number;
}

export function Tabs<T extends string>({
  current,
  items,
  onSelect,
  label,
}: {
  current: T;
  items: TabItem<T>[];
  onSelect: (id: T) => void;
  label: string;
}) {
  return (
    <div className="tabstrip" role="tablist" aria-label={label}>
      {items.map((t) => (
        <button
          key={t.id}
          type="button"
          role="tab"
          id={`tab-${t.id}`}
          aria-selected={current === t.id}
          aria-controls={`tabpanel-${t.id}`}
          className={current === t.id ? "tab on" : "tab"}
          onClick={() => onSelect(t.id)}
        >
          {t.label}
          {!!t.count && <span className="tab-count">{t.count}</span>}
        </button>
      ))}
    </div>
  );
}
