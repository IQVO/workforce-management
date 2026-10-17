/**
 * URL builders for workforce-management's supervisor endpoints.
 *
 * Extracted from WorkforceScreen so the exact endpoint shapes are unit-
 * tested in vitest's node environment (the web/ suite deliberately has no
 * jsdom/React plugin -- see vitest.config.ts) and so every screen change
 * that adopts or extends an endpoint carries its own regression tests.
 */

/**
 * The all-paths view on the DEPRECATED route: GET /buildings/{buildingId}/
 * shifts/{shiftId}/staffing-gap returns EVERY path planned in the committed
 * shift plan in one call (operationId listStaffingGapsForShift). Kept wired
 * because this remote has navigated by building since its first screen;
 * ADR 0035 supersedes it with siteShiftStaffingGapUrl, kept alongside here.
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
 * The CANONICAL per-shift view (ADR 0035): GET /sites/{siteCode}/shifts/
 * {shiftId}/staffing-gap lists every path planned in that site's committed
 * shift plan, with activeHeads scoped to associates on shift at that site
 * (ADR 0034). Same handler core and response shape as the deprecated
 * buildings route above.
 */
export function siteShiftStaffingGapUrl(
  base: string,
  siteCode: string,
  shiftId: string,
): string {
  return `${base}/sites/${encodeURIComponent(siteCode)}/shifts/${encodeURIComponent(
    shiftId,
  )}/staffing-gap`;
}

/**
 * The single-path lookup, kept as a deliberate narrow view: GET
 * /paths/{pathId}/staffing-gap?siteCode=&shiftId= (ShiftPlan is keyed by
 * site+shift, so both query params are required alongside the path).
 * siteCode is the canonical key (ADR 0035): it is both the plan lookup AND
 * the associate scope -- the response then echoes it back as siteCode.
 */
export function singlePathStaffingGapUrl(
  base: string,
  pathId: string,
  siteCode: string,
  shiftId: string,
): string {
  return `${base}/paths/${encodeURIComponent(pathId)}/staffing-gap?siteCode=${encodeURIComponent(
    siteCode,
  )}&shiftId=${encodeURIComponent(shiftId)}`;
}

/**
 * POST /associates/{id}/certifications (operationId certifyAssociate):
 * adds one named qualification to an already-started associate shift.
 */
export function certifyAssociateUrl(base: string, associateId: string): string {
  return `${base}/associates/${encodeURIComponent(associateId)}/certifications`;
}

/**
 * POST /associates/{id}/assignments (operationId assignLabor): puts the
 * associate on a path for a new interval, closing any active assignment
 * first (exactly one ACTIVE assignment per associate, by construction).
 */
export function assignLaborUrl(base: string, associateId: string): string {
  return `${base}/associates/${encodeURIComponent(associateId)}/assignments`;
}
