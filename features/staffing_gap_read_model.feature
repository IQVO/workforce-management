# Derived from:
# - apis/openapi.yaml — GET /paths/{pathId}/staffing-gap 404: "No
#   committed ShiftPlan exists for buildingId+shiftId
#   (resource-not-found)"; StaffingGapResponse documents understaffed.
# - .claude/rules/domain-model.md — PathUnderstaffed definition ("a
#   flag, not a decision: plannedHeads(path) not currently met by active
#   assignments") and use case 7 GetStaffingGap: "may raise
#   PathUnderstaffed" — only when active falls short of plan.
Feature: The staffing gap read model
  As a shift manager
  I want the gap read model to distinguish "no plan yet" from a real gap
  So that I only see PathUnderstaffed when a committed plan is genuinely unmet

  @bdd
  Scenario: A fully staffed path is not flagged PathUnderstaffed
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    And a ShiftPlan is committed for building "bldg-1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 1            | 30          | 8            | 10                |
    And associate "assoc-1" is assigned to path "pack"
    When the staffing gap for path "pack" is requested for building "bldg-1" shift "shift-1"
    Then path "pack" reports 1 planned heads and 1 active heads and is not understaffed

  @bdd
  Scenario: Requesting the staffing gap without a committed ShiftPlan is rejected
    When the staffing gap for path "pack" is requested for building "bldg-1" shift "shift-1"
    Then the request is rejected with status 404 and problem type "resource-not-found"
