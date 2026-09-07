import { ReactNode } from "react";
import { Kind, KIND_COLOR } from "./status";

// One thing that needs a decision. The same card appears on Home, in
// the Butler thread and on Plans — one component, three placements,
// which is what keeps "the Butler proposes but never acts" visible:
// the approve button is yours, wherever you meet the proposal.
//
// compact drops the body to a single line for the secondary problems
// that sit under the first one.
export function ProblemCard({
  severity,
  icon,
  title,
  meta,
  body,
  actions,
  aside,
  compact,
  children,
}: {
  severity: Kind;
  icon?: ReactNode;
  title: ReactNode;
  meta?: ReactNode;
  body?: ReactNode;
  actions?: ReactNode;
  aside?: ReactNode;
  compact?: boolean;
  // Detail that belongs to the decision — a plan's steps, a run's
  // history. It sits between the explanation and the buttons, because
  // it is what you read before you press one.
  children?: ReactNode;
}) {
  if (compact) {
    return (
      <div className="problem problem-compact">
        <span className="acc" style={{ background: KIND_COLOR[severity] }} aria-hidden />
        {icon}
        <div style={{ flex: 1, minWidth: 0 }}>{title}</div>
        {meta}
        {actions}
      </div>
    );
  }
  return (
    <div className="problem">
      <span className="acc" style={{ background: KIND_COLOR[severity], minHeight: 44 }} aria-hidden />
      <div style={{ flex: 1, minWidth: 0 }}>
        <div className="problem-head">
          {icon}
          <span className="problem-title">{title}</span>
          {meta}
        </div>
        {body && <p className="problem-body">{body}</p>}
        {children}
        {actions && <div className="problem-actions" style={children ? { marginTop: "var(--space-4)" } : undefined}>{actions}</div>}
      </div>
      {aside && <div className="problem-aside">{aside}</div>}
    </div>
  );
}
