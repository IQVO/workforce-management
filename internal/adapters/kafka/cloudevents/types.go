package cloudevents

// Entity segments of this service's `type` attribute — the aggregate that
// raises the event, exactly as already catalogued in apis/asyncapi.yaml.
const (
	EntityShiftPlan  = "shiftplan"
	EntityAssociate  = "associate"
	EntityAssignment = "assignment"
)

// Full CloudEvents `type` strings this service PUBLISHES. Consumers (this
// service's own analytics projector, and wes-work-planning for
// ShiftPlanCommitted) dispatch on these exact strings.
const (
	TypeShiftPlanCommitted    = "com.warehouse.wes.workforce-management.shiftplan.ShiftPlanCommitted"
	TypeShiftPlanProposed     = "com.warehouse.wes.workforce-management.shiftplan.ShiftPlanProposed"
	TypePathUnderstaffed      = "com.warehouse.wes.workforce-management.shiftplan.PathUnderstaffed"
	TypeAssociateShiftStarted = "com.warehouse.wes.workforce-management.associate.AssociateShiftStarted"
	TypeAssociateShiftEnded   = "com.warehouse.wes.workforce-management.associate.AssociateShiftEnded"
	TypeAssociateBreakStarted = "com.warehouse.wes.workforce-management.associate.AssociateBreakStarted"
	TypeAssociateBreakEnded   = "com.warehouse.wes.workforce-management.associate.AssociateBreakEnded"
	TypeAssociateCertified    = "com.warehouse.wes.workforce-management.associate.AssociateCertified"
	TypeLaborAssigned         = "com.warehouse.wes.workforce-management.assignment.LaborAssigned"
	TypeLaborReassigned       = "com.warehouse.wes.workforce-management.assignment.LaborReassigned"
)

// Full CloudEvents `type` strings this service CONSUMES from other bounded
// contexts (fleet standard §4 cross-service table). Byte-identical to the
// producers' constants; never a short name, never a suffix match.
const (
	TypeProcessPathCreated      = "com.warehouse.wes.process-path-management.processpath.ProcessPathCreated"
	TypeProcessPathUpdated      = "com.warehouse.wes.process-path-management.processpath.ProcessPathUpdated"
	TypeProcessPathDeactivated  = "com.warehouse.wes.process-path-management.processpath.ProcessPathDeactivated"
	TypeTaskPerformanceRecorded = "com.warehouse.wes.labor-performance.performance.TaskPerformanceRecorded"
)
