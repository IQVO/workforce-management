/** Wire types mirroring workforce-management's dto.go response shapes
 *  (staffingGapResponse, assignmentResponse) exactly -- kept hand-in-sync
 *  with the Go DTOs rather than code-generated for v1; revisit with
 *  openapi-typescript once the OpenAPI spec is the enforced source of
 *  truth. Optional fields are optional ON THE WIRE: absent means "no
 *  value", never a fabricated 0 or "". */
export interface StaffingGap {
  pathId: string;
  plannedHeads: number;
  activeHeads: number;
  understaffed: boolean;
  /** Observed idle share fed back from labor-performance
   *  (LABOR_PERFORMANCE_MODE=kafka-cache): a FRACTION in [0, 1], not a
   *  0-100 percentage. Omitted entirely when no idle-share signal is
   *  available -- render only when present. */
  observedIdleShare?: number;
  /** DEPRECATED wire duplicate of observedIdleShare: despite the "Pct"
   *  suffix it carries the same fraction in [0, 1]. Present exactly when
   *  observedIdleShare is. Kept on the type so the wire shape stays
   *  honest; the screen renders observedIdleShare. */
  observedIdlePct?: number;
  /** Echoed only when the query was scoped by site (siteCode given);
   *  absence means the count is unscoped/fleet-wide -- treat it as
   *  "unscoped", never as a particular site. */
  siteCode?: string;
}

/** POST /associates/{id}/certifications request body (CertifyRequest). */
export interface CertifyRequest {
  certification: string;
}

/** POST /associates/{id}/assignments request body (AssignLaborRequest). */
export interface AssignLaborRequest {
  pathId: string;
}

/** POST /associates/{id}/assignments 201 response (AssignmentResponse). */
export interface AssignmentResponse {
  associateId: string;
  /** The path of the current active assignment, if any. */
  activePathId?: string;
  active: boolean;
}

/** The fleet collection envelope: { items, nextCursor } (REST guideline 3.3).
 *  nextCursor is absent on the last page. */
export interface Page<T> {
  items: T[];
  nextCursor?: string;
}
