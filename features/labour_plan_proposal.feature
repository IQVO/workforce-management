# ADR 0043 (decisions 2 and 3): a proposal from warehouse-planning's published
# labour plan. warehouse-planning owns the requirement (its ADR 0023) and
# publishes LabourPlanCommitted per site, shift and path; this context keeps a
# local copy and proposes exactly those heads when the request omits charge
# and names shiftId (with the existing siteCode): rateSource labour-plan, no
# idle-share trim, and 404 labour-plan-not-found when nothing was published --
# never zero heads. A request with charge is the deprecated proposal and
# behaves exactly as before.
Feature: Path plan proposal from the published labour plan

  Scenario: Proposed heads are the published requirement, untrimmed
    # The idle-share step rebuilds the server, so it runs before the seed.
    Given the observed idle share for path "pack" is 0.9
    And warehouse-planning has published a labour plan of 6 heads at rate 25 for site "WH1" shift "shift-1" path "pack"
    When a POST request is sent to "/paths/pack/plan-proposals" with the body {"siteCode": "WH1", "shiftId": "shift-1"}
    Then the response status is 200
    And the response field "proposedHeads" is the number 6
    And the response field "resolvedRate" is the number 25
    And the response field "rateSource" is "labour-plan"
    And the response omits the field "trimReason"
    And 1 "ShiftPlanProposed" event was published

  Scenario Outline: No published labour plan is a 404, never zero heads
    Given warehouse-planning has published a labour plan of 6 heads at rate 25 for site "WH1" shift "shift-1" path "pack"
    When a POST request is sent to "/paths/<path>/plan-proposals" with the body {"siteCode": "<site>", "shiftId": "<shift>"}
    Then the request is rejected with status 404 and problem type "labour-plan-not-found"
    And 0 "ShiftPlanProposed" events were published

    Examples:
      | site | shift   | path |
      | WH2  | shift-1 | pack |
      | WH1  | shift-2 | pack |
      | WH1  | shift-1 | pick |

  Scenario: A request with a charge keeps the deprecated sizing even when it names a shift
    Given warehouse-planning has published a labour plan of 6 heads at rate 25 for site "WH1" shift "shift-1" path "pack"
    When a POST request is sent to "/paths/pack/plan-proposals" with the body {"siteCode": "WH1", "shiftId": "shift-1", "charge": 100, "plannedRate": 30}
    Then the response status is 200
    And the response field "proposedHeads" is the number 4
    And the response field "rateSource" is "caller"

  Scenario: Neither a charge nor a shift is still refused
    When a POST request is sent to "/paths/pack/plan-proposals" with the body {"siteCode": "WH1"}
    Then the request is rejected with status 400 and problem type "missing-charge"
