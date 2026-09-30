# Derived from:
# - apis/openapi.yaml — POST /shift-plans 409: "plannedHours exceeds what
#   plannedHeads can work within the shift's max hours
#   (planned-hours-exceed-capacity)".
# - .claude/rules/domain-model.md — "Aggregates & invariants"
#   (ShiftPlan): "PathPlan.plannedHours <= plannedHeads *
#   maxHoursPerShift is how 'sum of hours valid' is enforced on
#   ShiftPlan".
Feature: Planned hours are bounded by planned heads
  As a shift manager
  I want a committed plan's hours to fit within what its heads can work in one shift
  So that the plan never promises more labor hours than the headcount can deliver

  @bdd
  Scenario: Committing plannedHours beyond what planned heads can work is rejected
    When committing a ShiftPlan for building "bldg-1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 2            | 30          | 17           | 10                |
    Then the ShiftPlan commit is rejected with status 409 and problem type "planned-hours-exceed-capacity"
