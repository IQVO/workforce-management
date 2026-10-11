# ShiftPlan commit rules beyond the happy path.
#
# Derived from:
#   - apis/openapi.yaml, POST /shift-plans:
#       201 carrying the plan key under BOTH names (siteCode canonical,
#       buildingId its deprecated alias, same value); naturally idempotent by
#       the (siteCode, shiftId) key -- the repository upserts, a re-commit
#       replaces the plan and re-publishes ShiftPlanCommitted;
#       400 for an empty required field, no path plan lines
#       (shift-plan-no-path-plans), an undeclared path (unknown-path-id),
#       neither siteCode nor buildingId (missing-building-id);
#       409 planned-heads-exceed-installed / exceeds-installed-capacity /
#       planned-hours-exceed-capacity (plannedHours may not exceed
#       plannedHeads * max hours per shift);
#       422 conflicting-site-and-building when both names differ;
#       503 installed-capacity-unavailable: "This service fails the ENTIRE
#       commit loud ... rather than falling back to any assumed capacity"
#   - ADR-0014 (live installed-capacity ceiling), ADR 0035 (siteCode)

Feature: Committing a shift plan
  As a shift manager
  I want a plan committed whole or not at all, under one canonical key
  So that a rejected or unverifiable plan never half-exists

  @bdd
  Scenario: A plan keyed by siteCode is returned under both names
    When a POST request is sent to "/shift-plans" with the body {"siteCode": "WH1", "shiftId": "shift-1", "lines": [{"pathId": "pack", "plannedHeads": 2, "plannedRate": 30, "plannedHours": 16, "installedStations": 5}]}
    Then the last request succeeds with status 201
    And the response field "siteCode" is "WH1"
    And the response field "buildingId" is "WH1"
    And the response field "shiftId" is "shift-1"
    And the response field "lines.0.pathId" is "pack"
    And the response field "lines.0.plannedHeads" is the number 2
    And 1 "ShiftPlanCommitted" event was published

  @bdd
  Scenario: The deprecated buildingId alone is accepted and mirrored as the siteCode
    When a POST request is sent to "/shift-plans" with the body {"buildingId": "WH1", "shiftId": "shift-1", "lines": [{"pathId": "pack", "plannedHeads": 2, "plannedRate": 30, "plannedHours": 16, "installedStations": 5}]}
    Then the last request succeeds with status 201
    And the response field "siteCode" is "WH1"
    And the response field "buildingId" is "WH1"

  @bdd
  Scenario: Both names with the same value are accepted
    When a POST request is sent to "/shift-plans" with the body {"siteCode": "WH1", "buildingId": "WH1", "shiftId": "shift-1", "lines": [{"pathId": "pack", "plannedHeads": 2, "plannedRate": 30, "plannedHours": 16, "installedStations": 5}]}
    Then the last request succeeds with status 201

  @bdd
  Scenario: Both names with different values are a conflict
    When a POST request is sent to "/shift-plans" with the body {"siteCode": "WH1", "buildingId": "WH2", "shiftId": "shift-1", "lines": [{"pathId": "pack", "plannedHeads": 2, "plannedRate": 30, "plannedHours": 16, "installedStations": 5}]}
    Then the request is rejected with status 422 and problem type "conflicting-site-and-building"
    And 0 "ShiftPlanCommitted" events were published

  @bdd
  Scenario: A plan without any site key is rejected
    When a POST request is sent to "/shift-plans" with the body {"shiftId": "shift-1", "lines": [{"pathId": "pack", "plannedHeads": 2, "plannedRate": 30, "plannedHours": 16, "installedStations": 5}]}
    Then the request is rejected with status 400 and problem type "missing-building-id"

  @bdd
  Scenario: A plan without lines is rejected
    When a POST request is sent to "/shift-plans" with the body {"siteCode": "WH1", "shiftId": "shift-1", "lines": []}
    Then the request is rejected with status 400 and problem type "shift-plan-no-path-plans"
    And 0 "ShiftPlanCommitted" events were published

  @bdd
  Scenario: A malformed plan body is rejected
    When a POST request is sent to "/shift-plans" with the body {"siteCode": "WH1", "shiftId":
    Then the request is rejected with status 400 and problem type "malformed-request-body"

  @bdd
  Scenario Outline: Planned heads may equal the installed stations but not exceed them
    When a POST request is sent to "/shift-plans" with the body {"siteCode": "WH1", "shiftId": "shift-1", "lines": [{"pathId": "pack", "plannedHeads": <heads>, "plannedRate": 30, "plannedHours": 8, "installedStations": 5}]}
    Then the response status is <status>

    Examples:
      | heads | status |
      | 4     | 201    |
      | 5     | 201    |
      | 6     | 409    |

  @bdd
  Scenario Outline: Planned hours may fill the heads' shift capacity but not exceed it
    When a POST request is sent to "/shift-plans" with the body {"siteCode": "WH1", "shiftId": "shift-1", "lines": [{"pathId": "pack", "plannedHeads": 2, "plannedRate": 30, "plannedHours": <hours>, "installedStations": 5}]}
    Then the response status is <status>

    Examples:
      | hours | status |
      | 8     | 201    |
      | 16    | 201    |
      | 16.5  | 409    |

  @bdd
  Scenario: Planned heads may equal the live installed capacity but not exceed it
    Given fulfillment-execution has 4 stations registered with capability "pack"
    When a POST request is sent to "/shift-plans" with the body {"siteCode": "WH1", "shiftId": "shift-1", "lines": [{"pathId": "pack", "plannedHeads": 4, "plannedRate": 30, "plannedHours": 8, "installedStations": 10}]}
    Then the last request succeeds with status 201
    When a POST request is sent to "/shift-plans" with the body {"siteCode": "WH1", "shiftId": "shift-2", "lines": [{"pathId": "pack", "plannedHeads": 5, "plannedRate": 30, "plannedHours": 8, "installedStations": 10}]}
    Then the request is rejected with status 409 and problem type "exceeds-installed-capacity"

  @bdd
  Scenario: An unverifiable live capacity fails the whole commit
    Given fulfillment-execution cannot be reached
    When a POST request is sent to "/shift-plans" with the body {"siteCode": "WH1", "shiftId": "shift-1", "lines": [{"pathId": "pack", "plannedHeads": 2, "plannedRate": 30, "plannedHours": 8, "installedStations": 5}]}
    Then the request is rejected with status 503 and problem type "installed-capacity-unavailable"
    And 0 "ShiftPlanCommitted" events were published
    When the staffing gap for path "pack" is requested for building "WH1" shift "shift-1"
    Then the request is rejected with status 404 and problem type "resource-not-found"

  @bdd
  Scenario: A rejected plan is not stored
    When a POST request is sent to "/shift-plans" with the body {"siteCode": "WH1", "shiftId": "shift-1", "lines": [{"pathId": "pack", "plannedHeads": 9, "plannedRate": 30, "plannedHours": 8, "installedStations": 5}]}
    Then the request is rejected with status 409 and problem type "planned-heads-exceed-installed"
    When the staffing gap for path "pack" is requested for building "WH1" shift "shift-1"
    Then the request is rejected with status 404 and problem type "resource-not-found"

  @bdd
  Scenario: A second commit for the same site and shift replaces the plan
    Given a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 2            | 30          | 8            | 5                 |
    When a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 4            | 30          | 8            | 5                 |
    And the staffing gap for path "pack" is requested for building "WH1" shift "shift-1"
    Then path "pack" is flagged PathUnderstaffed with 4 planned heads and 0 active heads
    And 2 "ShiftPlanCommitted" events were published

  @bdd
  Scenario: Different shifts of the same site keep separate plans
    Given a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 2            | 30          | 8            | 5                 |
    And a ShiftPlan is committed for building "WH1" shift "shift-2" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 3            | 30          | 8            | 5                 |
    When the staffing gap for path "pack" is requested for building "WH1" shift "shift-1"
    Then path "pack" is flagged PathUnderstaffed with 2 planned heads and 0 active heads
    When the staffing gap for path "pack" is requested for building "WH1" shift "shift-2"
    Then path "pack" is flagged PathUnderstaffed with 3 planned heads and 0 active heads
