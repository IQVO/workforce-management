# Documentation-derived rules for the labour-requirement copy (ADR 0043).
#
# Derived from:
#   - ADR 0043 decision 1: the local copy of warehouse-planning's
#     LabourPlanCommitted applies a row "only when its version is greater
#     than the stored one", so a redelivered or reordered older message
#     never rolls the copy back (labourplan.Requirement.Supersedes; the
#     memory repo the suite runs on applies the same guard as Postgres)
#   - ADR 0043 decision 4 + .claude/rules/domain-model.md: "Committed lines
#     (and staffing-gap lines) carry an optional requiredHeads from the
#     labour-plan copy; a line is never refused for differing from it"
#   - business-context.md: "Committing fewer heads than the requirement is
#     not refused: it is a legitimate supply decision, and
#     warehouse-planning reports the difference as LabourPlanDriftDetected"
#   - apis/openapi.yaml: requiredHeads is "Omitted when nothing was
#     published. Never enforced: a line committing fewer heads than required
#     is accepted"; on the staffing gap "understaffed still compares
#     activeHeads with plannedHeads"
# The suite runs with MAX_HOURS_PER_SHIFT = 8 and the fleet catalogue.

Feature: The labour-requirement copy informs, never refuses
  As a shift supervisor
  I want warehouse-planning's published requirement shown next to my commitment
  So that I can decide supply with the demand in front of me, without the service deciding for me

  @bdd
  Scenario: A committed line shows the published requirement next to the commitment
    Given warehouse-planning has published a labour plan of 6 heads at rate 25 for site "WH1" shift "shift-1" path "pack"
    When a POST request is sent to "/shift-plans" with the body {"siteCode": "WH1", "shiftId": "shift-1", "lines": [{"pathId": "pack", "plannedHeads": 3, "plannedRate": 30, "plannedHours": 24, "installedStations": 5}]}
    Then the response status is 201
    And the response field "lines.0.plannedHeads" is the number 3
    And the response field "lines.0.requiredHeads" is the number 6

  @bdd
  Scenario: A committed line without a published requirement omits requiredHeads
    Given warehouse-planning has published a labour plan of 6 heads at rate 25 for site "WH1" shift "shift-1" path "pack"
    When a POST request is sent to "/shift-plans" with the body {"siteCode": "WH1", "shiftId": "shift-1", "lines": [{"pathId": "pack", "plannedHeads": 3, "plannedRate": 30, "plannedHours": 24, "installedStations": 5}, {"pathId": "pick", "plannedHeads": 2, "plannedRate": 40, "plannedHours": 16, "installedStations": 4}]}
    Then the response status is 201
    And the response field "lines.0.requiredHeads" is the number 6
    And the response omits the field "lines.1.requiredHeads"

  @bdd
  Scenario: Committing fewer heads than the requirement is a supply decision, not an error
    Given warehouse-planning has published a labour plan of 6 heads at rate 25 for site "WH1" shift "shift-1" path "pack"
    When a POST request is sent to "/shift-plans" with the body {"siteCode": "WH1", "shiftId": "shift-1", "lines": [{"pathId": "pack", "plannedHeads": 2, "plannedRate": 30, "plannedHours": 16, "installedStations": 5}]}
    Then the response status is 201
    And the response field "lines.0.plannedHeads" is the number 2
    And the response field "lines.0.requiredHeads" is the number 6
    And 1 "ShiftPlanCommitted" event was published

  @bdd
  Scenario: The staffing gap shows the requirement, but understaffed still compares against the commitment
    Given warehouse-planning has published a labour plan of 6 heads at rate 25 for site "WH1" shift "shift-1" path "pack"
    And a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 3            | 30          | 24           | 5                 |
    When a GET request is sent to "/paths/pack/staffing-gap?siteCode=WH1&shiftId=shift-1"
    Then the response status is 200
    And the response field "plannedHeads" is the number 3
    And the response field "requiredHeads" is the number 6
    And the response field "activeHeads" is the number 0
    And the response field "understaffed" is true

  @bdd
  Scenario: The staffing gap omits requiredHeads when nothing was published
    Given a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 3            | 30          | 24           | 5                 |
    When a GET request is sent to "/paths/pack/staffing-gap?siteCode=WH1&shiftId=shift-1"
    Then the response status is 200
    And the response field "plannedHeads" is the number 3
    And the response omits the field "requiredHeads"

  @bdd
  Scenario: The site's all-paths list carries the requirement line by line
    Given warehouse-planning has published a labour plan of 6 heads at rate 25 for site "WH1" shift "shift-1" path "pack"
    And a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 3            | 30          | 24           | 5                 |
      | pick   | 2            | 40          | 16           | 4                 |
    When a GET request is sent to "/sites/WH1/shifts/shift-1/staffing-gap"
    Then the response status is 200
    And the response is a list of 2 entries
    And the response field "items.0.pathId" is "pack"
    And the response field "items.0.requiredHeads" is the number 6
    And the response field "items.1.pathId" is "pick"
    And the response omits the field "items.1.requiredHeads"

  @bdd
  Scenario: A labour plan of a greater version replaces the copy
    Given warehouse-planning has published a labour plan of 4 heads at rate 25 for site "WH1" shift "shift-1" path "pack" as version 1
    And warehouse-planning has published a labour plan of 6 heads at rate 25 for site "WH1" shift "shift-1" path "pack" as version 2
    When a POST request is sent to "/paths/pack/plan-proposals" with the body {"siteCode": "WH1", "shiftId": "shift-1"}
    Then the response status is 200
    And the response field "proposedHeads" is the number 6
    And the response field "rateSource" is "labour-plan"

  @bdd
  Scenario Outline: A redelivered or reordered labour plan never rolls the copy back
    Given warehouse-planning has published a labour plan of 6 heads at rate 25 for site "WH1" shift "shift-1" path "pack" as version 2
    And a late labour plan of <heads> heads at rate <rate> for site "WH1" shift "shift-1" path "pack" is delivered as version <version>
    When a POST request is sent to "/paths/pack/plan-proposals" with the body {"siteCode": "WH1", "shiftId": "shift-1"}
    Then the response status is 200
    And the response field "proposedHeads" is the number 6
    And the response field "resolvedRate" is the number 25
    And the response field "rateSource" is "labour-plan"

    Examples:
      # an older message arriving after a newer one
      | heads | rate | version |
      | 4     | 20   | 1       |
      # the same version with different content is a no-op, not an update
      | 9     | 40   | 2       |
