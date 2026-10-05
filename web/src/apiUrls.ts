/**
 * URL builders for workforce-management's staffing-gap read model.
 *
 * Extracted from WorkforceScreen so the exact endpoint shapes are unit-
 * tested in vitest's node environment (the web/ suite deliberately has no
 * jsdom/React plugin -- see vitest.config.ts) and so the screen change
 * that adopted the all-paths endpoint (ADR-0011's fast-follow, shipped)
 * carries its own regression tests.
 */

/**
 * The all-paths view: GET /buildings/{buildingId}/shifts/{shiftId}/
 * staffing-gap returns EVERY path planned in the committed shift plan in
 * one call (operationId listStaffingGapsForShift).
 */
export function allPathsStaffingGapUrl(
  base: string,
  buildingId: string,
  shiftId: string,
): string {
  return `${base}/buildings/${encodeURIComponent(buildingId)}/shifts/${encodeURIComponent(
    shiftId,
  )}/staffing-gap`;
}

/**
 * The single-path lookup, kept as a deliberate narrow view: GET
 * /paths/{pathId}/staffing-gap?buildingId=&shiftId= (ShiftPlan is keyed by
 * building+shift, so both query params are required alongside the path).
 */
export function singlePathStaffingGapUrl(
  base: string,
  pathId: string,
  buildingId: string,
  shiftId: string,
): string {
  return `${base}/paths/${encodeURIComponent(pathId)}/staffing-gap?buildingId=${encodeURIComponent(
    buildingId,
  )}&shiftId=${encodeURIComponent(shiftId)}`;
}
