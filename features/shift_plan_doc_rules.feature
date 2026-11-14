# Documentation-derived rules for CommitShiftPlan.
#
# Derived from:
#   - business-context.md, Horizon 1: CommitShiftPlan "validates them as ONE
#     ATOMIC DECISION ... If any line fails, none of them commit"; capacity
#     "fails loud: if capacity cannot be verified, the whole commit is
#     rejected with 503"
#   - use-cases.md, CommitShiftPlan invariants: every line's path is in the
#     catalogue (unknown-path-id); the live installed-capacity ceiling is
#     looked up "once per capability per commit"; "Any
#     ErrInstalledCapacityUnavailable fails the WHOLE commit with 503";
#     planned hours within heads x MAX_HOURS_PER_SHIFT
#   - aggregate-design-canvas.md, ShiftPlan: identity is the pair
#     (siteCode, shiftId); "Committed --> Committed: CommitShiftPlan again for
#     the same site and shift"; "nothing is retried on the commit path"; a
#     rejected commit is "corrected by the human resubmitting"
#   - apis/openapi.yaml POST /shift-plans 400: "a line references a path with
#     no installedStations entry (shift-plan-missing-installed-stations)";
#     installedStations is a required field of every line
#   - domain-events.md: ShiftPlanCommitted {siteCode, shiftId}
# Harness: MAX_HOURS_PER_SHIFT = 8. Registering stations for ANY capability
# switches fulfillment-execution from "unlimited" to a registry in which an
# unregistered capability really counts 0 stations.

Feature: A shift plan is committed as one atomic, verified decision
  As a shift manager
  I want a plan committed whole or not at all, checked against live capacity
  So that heads are a finite pool that is never half-divided

  @bdd
  Scenario Outline: One bad line among good ones commits none of the plan
    Given fulfillment-execution has 5 stations registered with capability "pack"
    And fulfillment-execution has 8 stations registered with capability "pick"
    And fulfillment-execution has 3 stations registered with capability "rebin"
    And fulfillment-execution has 8 stations registered with capability "slam"
    When a POST request is sent to "/shift-plans" with the body {"siteCode": "WH1", "shiftId": "shift-1", "lines": [{"pathId": "pack", "plannedHeads": 2, "plannedRate": 30, "plannedHours": 16, "installedStations": 5}, {"pathId": "<path>", "plannedHeads": <heads>, "plannedRate": 30, "plannedHours": <hours>, "installedStations": <stations>}]}
    Then the request is rejected with status <status> and problem type "<problem>"
    And 0 "ShiftPlanCommitted" events were published
    When the staffing gap for path "pack" is requested for building "WH1" shift "shift-1"
    Then the request is rejected with status 404 and problem type "resource-not-found"

    Examples:
      | path     | heads | hours | stations | status | problem                        |
      | pick     | 6     | 8     | 5        | 409    | planned-heads-exceed-installed |
      | slam     | 2     | 17    | 5        | 409    | planned-hours-exceed-capacity  |
      | rebin    | 4     | 8     | 10       | 409    | exceeds-installed-capacity     |
      | teleport | 1     | 8     | 5        | 400    | unknown-path-id                |

  @bdd
  Scenario: A rejected re-commit leaves the committed plan in force
    Given a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 2            | 30          | 16           | 5                 |
    When a POST request is sent to "/shift-plans" with the body {"siteCode": "WH1", "shiftId": "shift-1", "lines": [{"pathId": "pack", "plannedHeads": 9, "plannedRate": 30, "plannedHours": 16, "installedStations": 5}]}
    Then the request is rejected with status 409 and problem type "planned-heads-exceed-installed"
    When the staffing gap for path "pack" is requested for building "WH1" shift "shift-1"
    Then path "pack" is flagged PathUnderstaffed with 2 planned heads and 0 active heads
    And 1 "ShiftPlanCommitted" event was published

  @bdd
  Scenario: A re-commit replaces every line, so a path left out is no longer planned
    Given a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 2            | 30          | 16           | 5                 |
      | pick   | 1            | 30          | 8            | 5                 |
    And a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 3            | 30          | 24           | 5                 |
    When a GET request is sent to "/sites/WH1/shifts/shift-1/staffing-gap"
    Then the response is a list of 1 entries
    And the response field "items.0.pathId" is "pack"
    And the response field "items.0.plannedHeads" is the number 3

  @bdd
  Scenario: The same shift id at two sites is two separate plans
    Given a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 2            | 30          | 16           | 5                 |
    And a ShiftPlan is committed for building "WH2" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 5            | 30          | 40           | 8                 |
    When the staffing gap for path "pack" is requested for building "WH1" shift "shift-1"
    Then path "pack" is flagged PathUnderstaffed with 2 planned heads and 0 active heads
    When the staffing gap for path "pack" is requested for building "WH2" shift "shift-1"
    Then path "pack" is flagged PathUnderstaffed with 5 planned heads and 0 active heads

  @bdd
  Scenario: Live capacity is looked up once per capability, however many paths share it
    Given fulfillment-execution has 6 stations registered with capability "pick"
    When a POST request is sent to "/shift-plans" with the body {"siteCode": "WH1", "shiftId": "shift-1", "lines": [{"pathId": "pick", "plannedHeads": 3, "plannedRate": 30, "plannedHours": 24, "installedStations": 5}, {"pathId": "pick-zone-a", "plannedHeads": 2, "plannedRate": 30, "plannedHours": 16, "installedStations": 5}]}
    Then the last request succeeds with status 201
    And fulfillment-execution was asked about capability "pick" 1 time

  @bdd
  Scenario: A capability with no registered station is a live ceiling of zero
    Given fulfillment-execution has 3 stations registered with capability "pick"
    When a POST request is sent to "/shift-plans" with the body {"siteCode": "WH1", "shiftId": "shift-1", "lines": [{"pathId": "REBIN", "plannedHeads": 1, "plannedRate": 30, "plannedHours": 8, "installedStations": 5}]}
    Then the request is rejected with status 409 and problem type "exceeds-installed-capacity"
    And 0 "ShiftPlanCommitted" events were published

  @bdd
  Scenario: One capability that cannot be verified fails the whole commit with 503
    Given fulfillment-execution has 5 stations registered with capability "pick"
    And fulfillment-execution cannot report the capacity of capability "slam"
    When a POST request is sent to "/shift-plans" with the body {"siteCode": "WH1", "shiftId": "shift-1", "lines": [{"pathId": "pick", "plannedHeads": 2, "plannedRate": 30, "plannedHours": 16, "installedStations": 5}, {"pathId": "slam", "plannedHeads": 1, "plannedRate": 30, "plannedHours": 8, "installedStations": 5}]}
    Then the request is rejected with status 503 and problem type "installed-capacity-unavailable"
    And 0 "ShiftPlanCommitted" events were published
    And fulfillment-execution was asked about capability "slam" 1 time
    When the staffing gap for path "pick" is requested for building "WH1" shift "shift-1"
    Then the request is rejected with status 404 and problem type "resource-not-found"

  @bdd
  Scenario: A commit whose capacity check fails is not retried
    Given fulfillment-execution cannot be reached
    When a POST request is sent to "/shift-plans" with the body {"siteCode": "WH1", "shiftId": "shift-1", "lines": [{"pathId": "pack", "plannedHeads": 2, "plannedRate": 30, "plannedHours": 16, "installedStations": 5}]}
    Then the request is rejected with status 503 and problem type "installed-capacity-unavailable"
    And fulfillment-execution was asked about capability "pack" 1 time

  @bdd
  Scenario: A plan without a shift id is rejected
    When a POST request is sent to "/shift-plans" with the body {"siteCode": "WH1", "lines": [{"pathId": "pack", "plannedHeads": 2, "plannedRate": 30, "plannedHours": 16, "installedStations": 5}]}
    Then the request is rejected with status 400 and problem type "missing-shift-id"
    And 0 "ShiftPlanCommitted" events were published

  @bdd
  Scenario Outline: Planned hours are bounded by the heads of each line, at any headcount
    When a POST request is sent to "/shift-plans" with the body {"siteCode": "WH1", "shiftId": "shift-1", "lines": [{"pathId": "pack", "plannedHeads": <heads>, "plannedRate": 30, "plannedHours": <hours>, "installedStations": 5}]}
    Then the response status is <status>

    Examples:
      | heads | hours | status |
      | 1     | 8     | 201    |
      | 1     | 8.5   | 409    |
      | 3     | 24    | 201    |
      | 3     | 25    | 409    |

  @bdd
  Scenario: The committed event names the site and the shift, also for the deprecated key
    When a POST request is sent to "/shift-plans" with the body {"buildingId": "WH1", "shiftId": "shift-7", "lines": [{"pathId": "pack", "plannedHeads": 2, "plannedRate": 30, "plannedHours": 16, "installedStations": 5}]}
    Then the last request succeeds with status 201
    And the last ShiftPlanCommitted event is for site "WH1" shift "shift-7"

  # Spec: openapi POST /shift-plans 400 lists "a line references a path with no
  # installedStations entry (shift-plan-missing-installed-stations)" and the
  # schema makes installedStations required. The handler decodes a missing
  # number as 0, so the line is answered 409 planned-heads-exceed-installed and
  # the 400 slug is unreachable from REST.
  @bdd @known-bug
  Scenario: A line that omits installedStations is refused as a missing count
    When a POST request is sent to "/shift-plans" with the body {"siteCode": "WH1", "shiftId": "shift-1", "lines": [{"pathId": "pack", "plannedHeads": 2, "plannedRate": 30, "plannedHours": 16}]}
    Then the request is rejected with status 400 and problem type "shift-plan-missing-installed-stations"
