# Documentation-derived rules for ProposePathPlan.
#
# Derived from:
#   - use-cases.md, ProposePathPlan (ADR 0012, ADR 0020):
#       * "When plannedRate is at most 0 and a MeasuredRateClient is wired, the
#         rate becomes 3600 / meanActualSeconds from labor-performance and
#         rateSource is measured. Otherwise rateSource is caller. When the
#         measured rate is unavailable, the rate stays at 0 and 0 heads are
#         proposed (fail-open)."
#       * a caller-supplied rate always wins (labor-performance is not asked)
#       * "When an IdleShareClient is wired and the idle share is above
#         IDLE_SHARE_TRIM_THRESHOLD (0.30), heads are trimmed to
#         ceil(heads x (1 - share)), never below 1. The response then carries
#         a trimReason."
#       * Raises ShiftPlanProposed (building, path, heads, resolved rate)
#   - business-context.md: the proposal "writes nothing"
#   - troubleshooting.md: 400 unknown-path-id
#   - openapi: plan-proposals 400 for an omitted charge (missing-charge)
# The idle-share "at or below the threshold" boundary is pinned in
# path_plan_proposal_rules.feature; this file pins the measured-rate policy and
# the trim above the threshold.

Feature: Proposed heads come from the documented rate and trim policies
  As a shift manager
  I want the proposal to say where its rate came from and why heads were trimmed
  So that I commit a plan knowing what the number is made of

  @bdd
  Scenario Outline: Without a caller rate the measured rate is 3600 over the mean seconds per task
    Given labor-performance reports a mean of <seconds> seconds per task for path "pack"
    When a POST request is sent to "/paths/pack/plan-proposals" with the body {"siteCode": "WH1", "charge": 100}
    Then the last request succeeds with status 200
    And the response field "rateSource" is "measured"
    And the response field "resolvedRate" is the number <rate>
    And the response field "proposedHeads" is the number <heads>
    And the last ShiftPlanProposed event is for site "WH1" path "pack" with <heads> heads at rate <rate>

    Examples:
      | seconds | rate | heads |
      | 120     | 30   | 4     |
      | 90      | 40   | 3     |
      | 45      | 80   | 2     |
      | 3600    | 1    | 100   |

  @bdd
  Scenario Outline: A caller rate of zero or less is not a supplied rate
    Given labor-performance reports a mean of 120 seconds per task for path "pack"
    When a POST request is sent to "/paths/pack/plan-proposals" with the body {"siteCode": "WH1", "charge": 100, "plannedRate": <rate>}
    Then the response field "rateSource" is "measured"
    And the response field "proposedHeads" is the number 4

    Examples:
      | rate |
      | 0    |
      | -5   |

  @bdd
  Scenario: A caller rate always wins and labor-performance is not asked
    Given labor-performance reports a mean of 120 seconds per task for path "pack"
    When a POST request is sent to "/paths/pack/plan-proposals" with the body {"siteCode": "WH1", "charge": 100, "plannedRate": 20}
    Then the response field "rateSource" is "caller"
    And the response field "resolvedRate" is the number 20
    And the response field "proposedHeads" is the number 5
    And labor-performance was consulted 0 times

  @bdd
  Scenario: The measured rate is the one measured for the path being proposed
    Given labor-performance reports a mean of 120 seconds per task for path "pack"
    When a POST request is sent to "/paths/pick/plan-proposals" with the body {"siteCode": "WH1", "charge": 100}
    Then the response field "rateSource" is "caller"
    And the response field "proposedHeads" is the number 0

  @bdd
  Scenario: When no measured rate is available the proposal fails open to zero heads
    Given labor-performance has no measured rate for path "pack"
    When a POST request is sent to "/paths/pack/plan-proposals" with the body {"siteCode": "WH1", "charge": 100}
    Then the last request succeeds with status 200
    And the response field "rateSource" is "caller"
    And the response field "resolvedRate" is the number 0
    And the response field "proposedHeads" is the number 0
    And the last ShiftPlanProposed event is for site "WH1" path "pack" with 0 heads at rate 0

  @bdd
  Scenario Outline: A measured duration that cannot be converted counts as no measured rate
    Given labor-performance reports a mean of <seconds> seconds per task for path "pack"
    When a POST request is sent to "/paths/pack/plan-proposals" with the body {"siteCode": "WH1", "charge": 100}
    Then the response field "rateSource" is "caller"
    And the response field "proposedHeads" is the number 0

    Examples:
      | seconds |
      | 0       |
      | -10     |

  @bdd
  Scenario: The idle-share trim applies to a measured rate too
    Given labor-performance reports a mean of 120 seconds per task for path "pack"
    And the observed idle share for path "pack" is 0.5
    When a POST request is sent to "/paths/pack/plan-proposals" with the body {"siteCode": "WH1", "charge": 100}
    Then the response field "rateSource" is "measured"
    And the response field "proposedHeads" is the number 2
    And the response field "trimReason" contains "heads trimmed from 4 to 2"

  @bdd
  Scenario Outline: Above the threshold the heads shrink to the ceiling of heads times what is not idle
    Given the observed idle share for path "pack" is <share>
    When a POST request is sent to "/paths/pack/plan-proposals" with the body {"siteCode": "WH1", "charge": 100, "plannedRate": 10}
    Then the response field "proposedHeads" is the number <heads>
    And the response field "trimReason" contains "heads trimmed from 10 to <heads>"

    Examples:
      | share | heads |
      | 0.31  | 7     |
      | 0.5   | 5     |
      | 0.75  | 3     |
      | 0.95  | 1     |
      | 1     | 1     |

  @bdd
  Scenario: With no idle observation yet the proposal is not trimmed
    Given no idle share has been observed for path "pack"
    When a POST request is sent to "/paths/pack/plan-proposals" with the body {"siteCode": "WH1", "charge": 100, "plannedRate": 10}
    Then the last request succeeds with status 200
    And the response field "proposedHeads" is the number 10
    And the response omits the field "trimReason"

  @bdd
  Scenario: The proposal event carries the trimmed heads and the resolved rate
    Given the observed idle share for path "pack" is 0.5
    When a POST request is sent to "/paths/pack/plan-proposals" with the body {"buildingId": "WH1", "charge": 100, "plannedRate": 10}
    Then the last ShiftPlanProposed event is for site "WH1" path "pack" with 5 heads at rate 10

  @bdd
  Scenario: A proposal for a path the catalogue does not declare is rejected
    When a POST request is sent to "/paths/teleport/plan-proposals" with the body {"siteCode": "WH1", "charge": 100, "plannedRate": 10}
    Then the request is rejected with status 400 and problem type "unknown-path-id"
    And 0 "ShiftPlanProposed" events were published

  @bdd
  Scenario: An omitted charge is named as the missing charge
    When a POST request is sent to "/paths/pack/plan-proposals" with the body {"siteCode": "WH1", "plannedRate": 10}
    Then the request is rejected with status 400 and problem type "missing-charge"
