import { useEffect, useState, type FormEvent } from "react";
import { fetchAllPages } from "../api";
import { WORKFORCE_API_BASE } from "../config";
import {
  allPathsStaffingGapUrl,
  singlePathStaffingGapUrl,
  siteShiftStaffingGapUrl,
} from "../apiUrls";
import type { StaffingGap } from "../types";
import { Card, StatusPill, formatNumber, useFetch } from "@warehouse/ui-kit";

/**
 * Staffing-gap dashboard. Three views over the same committed shift plan:
 *
 * 1. SHIFT, BY SITE (the default, canonical since ADR 0035): GET
 *    /sites/{siteCode}/shifts/{shiftId}/staffing-gap lists every path
 *    planned in that site's committed plan in one call, with activeHeads
 *    scoped to associates on shift at that site (ADR 0034).
 * 2. SHIFT, BY BUILDING (kept): the deprecated GET /buildings/{buildingId}/
 *    shifts/{shiftId}/staffing-gap alias this remote has always used --
 *    same handler core, but a building-only call is NOT scoped by site.
 * 3. BY PATH (kept): GET /paths/{pathId}/staffing-gap?siteCode=&shiftId=
 *    narrows to one caller-supplied path (siteCode is the canonical plan
 *    key and scope since ADR 0035).
 *
 * All three endpoints compute a path's gap identically, so the views can
 * never disagree for a given path. Each card also shows the gap itself
 * (planned minus present heads) and whatever measured-rate enrichment the
 * response carries (observedIdleShare, siteCode echo) -- omitted wire
 * fields are never rendered as fabricated zeros.
 */
type GapMode = "site" | "building" | "path";

export function WorkforceScreen() {
  const [mode, setMode] = useState<GapMode>("site");
  const [pathIdInput, setPathIdInput] = useState("");
  const [siteCodeInput, setSiteCodeInput] = useState("WH1");
  const [buildingIdInput, setBuildingIdInput] = useState("wh1");
  const [shiftIdInput, setShiftIdInput] = useState("shift-1");
  const [query, setQuery] = useState<{
    mode: GapMode;
    pathId: string;
    siteCode: string;
    buildingId: string;
    shiftId: string;
  } | null>(null);

  const url = query
    ? query.mode === "site"
      ? siteShiftStaffingGapUrl(WORKFORCE_API_BASE, query.siteCode, query.shiftId)
      : query.mode === "building"
        ? allPathsStaffingGapUrl(WORKFORCE_API_BASE, query.buildingId, query.shiftId)
        : singlePathStaffingGapUrl(WORKFORCE_API_BASE, query.pathId, query.siteCode, query.shiftId)
    : null;
  // The two shift views answer with a {items,nextCursor} page (followed to
  // the end by fetchAllPages); the single-path lookup with one object.
  const isList = query !== null && query.mode !== "path";
  const single = useFetch<StaffingGap>(isList ? null : url);
  const [list, setList] = useState<{
    data: StaffingGap[];
    loading: boolean;
    error: Error | null;
  }>({ data: [], loading: false, error: null });
  useEffect(() => {
    if (!isList || !url) return;
    const ctrl = new AbortController();
    setList({ data: [], loading: true, error: null });
    fetchAllPages<StaffingGap>(url, ctrl.signal)
      .then((data) => setList({ data, loading: false, error: null }))
      .catch((e: unknown) => {
        if (ctrl.signal.aborted) return;
        setList({ data: [], loading: false, error: e instanceof Error ? e : new Error(String(e)) });
      });
    return () => ctrl.abort();
  }, [isList, url]);
  const loading = isList ? list.loading : single.loading;
  const error = isList ? list.error : single.error;
  const gaps: StaffingGap[] = isList ? list.data : single.data ? [single.data] : [];

  function onSubmit(e: FormEvent) {
    e.preventDefault();
    const pathId = pathIdInput.trim();
    const siteCode = siteCodeInput.trim();
    const buildingId = buildingIdInput.trim();
    const shiftId = shiftIdInput.trim();
    if (!shiftId) {
      return;
    }
    if (mode === "site" && !siteCode) {
      return;
    }
    if (mode === "building" && !buildingId) {
      return;
    }
    if (mode === "path" && (!pathId || !siteCode)) {
      return;
    }
    setQuery({ mode, pathId, siteCode, buildingId, shiftId });
  }

  const maxHeads = gaps.reduce((m, g) => Math.max(m, g.plannedHeads, g.activeHeads), 1);
  const understaffedCount = gaps.filter((g) => g.understaffed).length;
  const scopeLabel =
    query === null
      ? ""
      : query.mode === "building"
        ? `Building: ${query.buildingId}`
        : `Site: ${query.siteCode}`;

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "var(--wh-space-5)" }}>
      <div>
        <h1 style={{ fontSize: "var(--wh-font-size-2xl)", margin: 0 }}>Workforce</h1>
        <p style={{ color: "var(--wh-color-text-muted)", marginTop: 4 }}>
          workforce-management · staffing gap by path -- planned vs present headcount
        </p>
      </div>

      <form onSubmit={onSubmit} style={{ display: "flex", gap: "var(--wh-space-2)", flexWrap: "wrap" }}>
        <select
          value={mode}
          onChange={(e) => setMode(e.target.value as GapMode)}
          style={inputStyle({ width: 190 })}
          aria-label="View mode"
        >
          <option value="site">Shift (by site)</option>
          <option value="building">Shift (by building)</option>
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
        {mode !== "building" && (
          <input
            value={siteCodeInput}
            onChange={(e) => setSiteCodeInput(e.target.value)}
            placeholder="Site code"
            style={inputStyle({ width: 140 })}
          />
        )}
        {mode === "building" && (
          <input
            value={buildingIdInput}
            onChange={(e) => setBuildingIdInput(e.target.value)}
            placeholder="Building ID"
            style={inputStyle({ width: 140 })}
          />
        )}
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
            Enter a site and shift to see planned vs present headcount for every path in
            the committed plan, narrow to one path, or use the legacy building-keyed view.
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
            No committed shift plan found for this {query.mode === "building" ? "building" : "site"}/shift with paths to show.
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
            <span>{scopeLabel}</span>
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
  const headGap = gap.plannedHeads - gap.activeHeads;
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
          label="Present heads"
          value={gap.activeHeads}
          max={maxHeads}
          tone={gap.understaffed ? "warning" : "success"}
        />
        <div
          style={{
            display: "flex",
            gap: "var(--wh-space-6)",
            fontSize: "var(--wh-font-size-sm)",
            color: "var(--wh-color-text-muted)",
            flexWrap: "wrap",
          }}
        >
          <span>
            Gap:{" "}
            <span
              style={{
                fontWeight: 600,
                fontFamily: "var(--wh-font-mono)",
                color: gap.understaffed
                  ? "var(--wh-color-status-warning)"
                  : "var(--wh-color-text)",
              }}
            >
              {headGap > 0 ? `+${headGap}` : formatNumber(headGap)}
            </span>
          </span>
          {/* Enrichment the response actually carries; omitted wire fields
              are absent on purpose (no idle-share signal, unscoped query)
              and must not render as fabricated zeros. */}
          {gap.observedIdleShare != null && (
            <span>
              Observed idle:{" "}
              <span style={{ fontFamily: "var(--wh-font-mono)" }}>
                {formatNumber(gap.observedIdleShare * 100)}%
              </span>
            </span>
          )}
          {gap.siteCode != null && <span>Site: {gap.siteCode}</span>}
        </div>
      </div>
    </Card>
  );
}

/** Simple two-number visual comparison (planned vs present) using inline
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
