import { ArrowSquareOut } from "@phosphor-icons/react";
import { Service } from "../services";
import { Kind } from "./status";
import { StatusPill, Dot } from "./status";
import { Sparkline } from "./Sparkline";

// A service as the household meets it: a name, where it answers, and
// whether it is up. The tile opens the detail drawer; only the corner
// arrow leaves the console.
export function ServiceTile({
  service,
  samples,
  onOpen,
}: {
  service: Service;
  samples?: Kind[];
  onOpen?: () => void;
}) {
  const { name, node, url, health, state } = service;
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
      {url && (
        <a
          className="launch"
          href={url}
          target="_blank"
          rel="noreferrer"
          onClick={(e) => e.stopPropagation()}
          aria-label={`Open ${name} in a new tab`}
          title={`Open ${url}`}
        >
          <ArrowSquareOut size={14} />
        </a>
      )}
      <div className="tile-head">
        <span className="tile-name">{name}</span>
      </div>
      <div className="tile-host tnum" title={url}>
        {url ? hostOf(url) : "internal only"}
      </div>
      <div className="tile-foot">
        <Dot kind={health} />
        <StatusPill kind={health}>{state}</StatusPill>
      </div>
      <div className="tile-meta">{node && <span>{node}</span>}</div>
      {samples && <Sparkline samples={samples} height={12} />}
    </div>
  );
}

function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}
