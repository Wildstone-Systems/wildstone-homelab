import { ReactNode } from "react";
import { ArrowSquareOut } from "@phosphor-icons/react";
import { Kind, StatusPill, Dot } from "./status";
import { Sparkline } from "./Sparkline";

// A service as the household thinks of it: a name, what it does, where
// it answers. The tile opens the detail drawer; only the corner arrow
// leaves the console.
export function ServiceTile({
  name,
  what,
  host,
  health,
  state,
  resourceCount,
  icon,
  samples,
  onOpen,
  launchHref,
}: {
  name: string;
  what?: string;
  host?: string;
  health: Kind;
  state: string;
  resourceCount: number;
  icon?: ReactNode;
  samples?: Kind[];
  onOpen?: () => void;
  launchHref?: string;
}) {
  return (
    <div
      className="tile"
      role="button"
      tabIndex={0}
      onClick={onOpen}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onOpen?.();
        }
      }}
      aria-label={`${name} — ${state}`}
    >
      {launchHref && (
        <a
          className="launch"
          href={launchHref}
          target="_blank"
          rel="noreferrer"
          onClick={(e) => e.stopPropagation()}
          aria-label={`Open ${name} in a new tab`}
          title={`Open ${name}`}
        >
          <ArrowSquareOut size={14} />
        </a>
      )}
      <div className="tile-head">
        {icon}
        <span className="tile-name">{name}</span>
      </div>
      {what && <div className="tile-what">{what}</div>}
      <div className="tile-foot">
        <Dot kind={health} />
        <StatusPill kind={health}>{state}</StatusPill>
      </div>
      <div className="tile-meta">
        <span>
          {resourceCount} {resourceCount === 1 ? "resource" : "resources"}
        </span>
        {host && <span title={host}>{host}</span>}
      </div>
      {samples && <Sparkline samples={samples} height={12} />}
    </div>
  );
}
