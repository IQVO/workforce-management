# Documentation-derived rule for same-path reassignment.
#
# Derived from:
#   - docs use-cases.md (warehouse-docs copy), AssignLabor "Raises":
#     "LaborReassigned (from path -> to path) whenever an active assignment
#     was closed to open the new one. That includes assigning the path the
#     associate is already on: the interval is closed and reopened and the
#     event carries fromPathId == toPathId (LaborAssignment.AssignBy has no
#     same-path check)"
#   - domain-events.md: LaborReassigned "an active assignment was closed in
#     favour of a new one (another path, or the same path again: fromPathId
#     may equal toPathId)"
#   - aggregate-design-canvas.md, LaborAssignment: "At most one ACTIVE
#     assignment per associate ... structural — Assign closes the active
#     interval before opening a new one" — so after the same-path move the
#     path still counts exactly one active head.
# The suite runs with MAX_HOURS_PER_SHIFT = 8 and the fleet catalogue.

Feature: Reassigning within the same path is a close and reopen, not a no-op
  As a shift manager
  I want re-asserting an associate's current path to close and reopen their interval
  So that the assignment history and the LaborReassigned event tell the truth about every move

  @bdd
  Scenario: Assigning the path the associate is already on raises a reassignment onto itself
    Given an AssociateShift is started for associate "assoc-1" with certifications "pick"
    And a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pick   | 2            | 40          | 16           | 4                 |
    And associate "assoc-1" is assigned to path "pick"
    Then the last request succeeds with status 201
    And 1 "LaborAssigned" event was published
    When associate "assoc-1" is assigned to path "pick"
    Then the last request succeeds with status 201
    And 1 "LaborAssigned" event was published
    And the last LaborReassigned event moved associate "assoc-1" from path "pick" to path "pick"
    And path "pick" has 1 active head in building "WH1" shift "shift-1"
