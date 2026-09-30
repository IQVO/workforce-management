# Derived from:
# - apis/openapi.yaml — POST /associates/{id}/end-shift: "Any currently
#   active LaborAssignment is closed first (its hours logged against the
#   shift). Idempotent: ending an already-ended shift is a no-op — no
#   error, no event, still 204."; POST /associates/{id}/assignments 409
#   associate-shift-ended.
# - .claude/rules/domain-model.md — "Aggregates & invariants"
#   (AssociateShift) and use case 8 EndAssociateShift: "closes all active
#   assignments, raises AssociateShiftEnded".
Feature: Ending an associate's shift
  As a shift manager
  I want ending a shift to close the associate's roster entry and any active assignment
  So that logged hours land on the shift and the associate leaves the labor picture

  @bdd
  Scenario: Ending a shift closes the associate's active LaborAssignment
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    And a ShiftPlan is committed for building "bldg-1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 1            | 30          | 8            | 10                |
    And associate "assoc-1" is assigned to path "pack"
    When associate "assoc-1" ends their shift
    Then the last request succeeds with status 204
    And path "pack" has 0 active heads in building "bldg-1" shift "shift-1"

  @bdd
  Scenario: Ending an already-ended shift is an idempotent no-op
    Given an AssociateShift is started for associate "assoc-2" with no certifications
    And associate "assoc-2" has ended their shift
    When associate "assoc-2" ends their shift
    Then the last request succeeds with status 204

  @bdd
  Scenario: Assigning an associate whose shift has ended is rejected
    Given an AssociateShift is started for associate "assoc-3" with certifications "pack"
    And associate "assoc-3" has ended their shift
    When associate "assoc-3" is assigned to path "pack"
    Then the request is rejected with status 409 and problem type "associate-shift-ended"
