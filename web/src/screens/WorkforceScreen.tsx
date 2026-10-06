import { useState, type FormEvent } from "react";
import { WORKFORCE_API_BASE } from "../config";
import {
  allPathsStaffingGapUrl,
  singlePathStaffingGapUrl,
} from "../apiUrls";
import type { StaffingGap } from "../types";
import { Card, StatusPill, useFetch } from "@warehouse/ui-kit";

/**
 * Staffing-gap dashboard. Two views over the same committed shift plan:
 *
 * 1. ALL PATHS (the default): GET /buildings/{buildingId}/shifts/{shiftId}/
 *    staffing-gap lists every path planned in the plan in one call — the
 *    "fleet-wide all-paths" endpoint ADR-0011 originally deferred and later
 *    shipped (operationId listStaffingGapsForShift). One building/shift
 *    pair, one request, every path's gap.
 * 2. BY PATH (kept): GET /paths/{pathId}/staffing-gap?buildingId=&shiftId=
 *    narrows to one caller-supplied path. ShiftPlan is keyed by
 *    building+shift, so both query params are required alongside the path.
 *
 * Both endpoints compute a path's gap identically, so the two views can
 * never disagree for a given path.
 */
export function WorkforceScreen() {
  const [mode, setMode] = useState<"all" | "path">("all");
  const [pathIdInput, setPathIdInput] = useState("");
  const [buildingIdInput, setBuildingIdInput] = useState("wh1");
  const [shiftIdInput, setShiftIdInput] = useState("shift-1");
  const [query, setQuery] = useState<{
    mode: "all" | "path";
    pathId: string;
    buildingId: string;
    shiftId: string;
  } | null>(null);

  const url = query
    ? query.mode === "all"
      ? allPathsStaffingGapUrl(WORKFORCE_API_BASE, query.buildingId, query.shiftId)
      : singlePathStaffingGapUrl(WORKFORCE_API_BASE, query.pathId, query.buildingId, query.shiftId)
    : null;
  // The all-paths endpoint answers with an array; the single-path lookup
  // with one object. Normalise both to a list for rendering.
  const { data, loading, error } = useFetch<StaffingGap[] | StaffingGap>(url);
  const gaps: StaffingGap[] = !data
    ? []
    : Array.isArray(data)
      ? data
      : [data];

  function onSubmit(e: FormEvent) {
    e.preventDefault();
    const pathId = pathIdInput.trim();
    const buildingId = buildingIdInput.trim();
    const shiftId = shiftIdInput.trim();
    if (!buildingId || !shiftId) {
      return;
    }
    if (mode === "path" && !pathId) {
      return;
    }
    setQuery({ mode, pathId, buildingId, shiftId });
  }

  const maxHeads = gaps.reduce((m, g) => Math.max(m, g.plannedHeads, g.activeHeads), 1);
  const understaffedCount = gaps.filter((g) => g.understaffed).length;

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "var(--wh-space-5)" }}>
      <div>
        <h1 style={{ fontSize: "var(--wh-font-size-2xl)", margin: 0 }}>Workforce</h1>
        <p style={{ color: "var(--wh-color-text-muted)", marginTop: 4 }}>
          workforce-management · staffing gap by path -- planned vs active headcount
        </p>
      </div>

      <form onSubmit={onSubmit} style={{ display: "flex", gap: "var(--wh-space-2)", flexWrap: "wrap" }}>
        <select
          value={mode}
          onChange={(e) => setMode(e.target.value as "all" | "path")}
          style={inputStyle({ width: 130 })}
          aria-label="View mode"
        >
          <option value="all">All paths</option>
          <option value="path">By path</option>
        </select>
        {mode === "path" && (
          <input
            value={pathIdInput}
            onChange={(e) => setPathIdInput(e.target.value)}
            placeholder="Path ID"
            style={inputStyle({ flex: 1, minWidth: 160 })}
          />
        )}
        <input
          value={buildingIdInput}
          onChange={(e) => setBuildingIdInput(e.target.value)}
          placeholder="Building ID"
          style={inputStyle({ width: 140 })}
        />
        <input
          value={shiftIdInput}
          onChange={(e) => setShiftIdInput(e.target.value)}
          placeholder="Shift ID"
          style={inputStyle({ width: 140 })}
        />
        <button type="submit" style={buttonStyle}>
          Check gap
        </button>
      </form>

      {!query && (
        <Card>
          <div style={{ color: "var(--wh-color-text-muted)" }}>
            Enter a building and shift to see planned vs active headcount for every
            path in the committed plan, or switch to “By path” for one path.
          </div>
        </Card>
      )}

      {error && (
        <Card>
          <div style={{ color: "var(--wh-color-status-danger)" }}>{error.message}</div>
        </Card>
      )}

      {loading && query && (
        <Card title={query.mode === "path" ? query.pathId : "All paths"}>
          <div style={{ color: "var(--wh-color-text-muted)" }}>Loading staffing gap…</div>
        </Card>
      )}

      {!loading && query && gaps.length === 0 && !error && (
        <Card>
          <div style={{ color: "var(--wh-color-text-muted)" }}>
            No committed shift plan found for this building/shift with paths to show.
          </div>
        </Card>
      )}

      {!loading && gaps.length > 0 && (
        <>
          <div
            style={{
              display: "flex",
              gap: "var(--wh-space-6)",
              fontSize: "var(--wh-font-size-sm)",
              color: "var(--wh-color-text-muted)",
            }}
          >
            <span>Building: {query?.buildingId}</span>
            <span>Shift: {query?.shiftId}</span>
            <span>
              {gaps.length} path{gaps.length === 1 ? "" : "s"} ·{" "}
              {understaffedCount === 0
                ? "all staffed"
                : `${understaffedCount} understaffed`}
            </span>
          </div>
          {gaps.map((gap) => (
            <PathGapCard key={gap.pathId} gap={gap} maxHeads={maxHeads} />
          ))}
        </>
      )}
    </div>
  );
}

function PathGapCard({ gap, maxHeads }: { gap: StaffingGap; maxHeads: number }) {
  const status = gap.understaffed ? "Understaffed" : "Staffed";
  return (
    <Card
      title={gap.pathId}
      actions={
        <StatusPill status={status} tone={gap.understaffed ? "warning" : "success"} />
      }
    >
      <div style={{ display: "flex", flexDirection: "column", gap: "var(--wh-space-4)" }}>
        <HeadcountBar label="Planned heads" value={gap.plannedHeads} max={maxHeads} tone="neutral" />
        <HeadcountBar
          label="Active heads"
          value={gap.activeHeads}
          max={maxHeads}
          tone={gap.understaffed ? "warning" : "success"}
        />
      </div>
    </Card>
  );
}

/** Simple two-number visual comparison (planned vs active) using inline
 *  styled divs matching the design tokens -- deliberately not a new
 *  ui-kit chart component for this single pilot dashboard; revisit if a
 *  second screen needs the same shape. */
function HeadcountBar({
  label,
  value,
  max,
  tone,
}: {
  label: string;
  value: number;
  max: number;
  tone: "neutral" | "success" | "warning";
}) {
  const pct = max > 0 ? Math.min(100, (value / max) * 100) : 0;
  const fg =
    tone === "success"
      ? "var(--wh-color-status-success)"
      : tone === "warning"
        ? "var(--wh-color-status-warning)"
        : "var(--wh-color-text-muted)";
  const bg =
    tone === "success"
      ? "var(--wh-color-status-success-bg)"
      : tone === "warning"
        ? "var(--wh-color-status-warning-bg)"
        : "var(--wh-color-bg-sunken)";

  return (
    <div>
      <div
        style={{
          display: "flex",
          justifyContent: "space-between",
          fontSize: "var(--wh-font-size-sm)",
          marginBottom: 6,
        }}
      >
        <span>{label}</span>
        <span style={{ fontWeight: 600, fontFamily: "var(--wh-font-mono)" }}>{value}</span>
      </div>
      <div
        style={{
          height: 10,
          borderRadius: "var(--wh-radius-pill)",
          background: bg,
          overflow: "hidden",
        }}
      >
        <div
          style={{
            height: "100%",
            width: `${pct}%`,
            background: fg,
            borderRadius: "var(--wh-radius-pill)",
            transition: "width 200ms ease",
          }}
        />
      </div>
    </div>
  );
}

function inputStyle(extra: Record<string, string | number>) {
  return {
    padding: "10px 12px",
    borderRadius: "var(--wh-radius-md)",
    border: "1px solid var(--wh-color-border)",
    background: "var(--wh-color-bg-sunken)",
    color: "var(--wh-color-text)",
    fontFamily: "var(--wh-font-mono)",
    fontSize: "var(--wh-font-size-sm)",
    ...extra,
  } as const;
}

const buttonStyle = {
  padding: "10px 18px",
  borderRadius: "var(--wh-radius-md)",
  border: "none",
  background: "var(--wh-color-accent)",
  color: "#fff",
  fontWeight: 600,
  fontSize: "var(--wh-font-size-sm)",
  cursor: "pointer",
} as const;
