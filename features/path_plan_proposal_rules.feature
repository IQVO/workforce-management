# Path plan proposal rules.
#
# Derived from:
#   - apis/openapi.yaml, POST /paths/{pathId}/plan/propose: "Computes
#     heads = ceil(charge / plannedRate) for one path and raises
#     ShiftPlanProposed for visibility -- it persists nothing and commits
#     nothing"; 200 (not 201: no resource is created); a high observed idle
#     share above IDLE_SHARE_TRIM_THRESHOLD (default 0.30) trims the heads,
#     "floored at 1 head minimum", with trimReason explaining why --
#     "fail-open: no idle-share signal available applies no trim"; 400; 422
#     conflicting-site-and-building
#   - ProposePathPlan: plannedRate is OPTIONAL (<= 0 means "not supplied");
#     charge is required (an omitted charge is a client mistake, an explicit
#     0 is a legitimate "nothing to do")

Feature: Proposing heads for a path
  As a shift manager
  I want a pure proposal I can look at before committing anything
  So that a suggestion never becomes a plan by accident

  @bdd
  Scenario Outline: Heads are the ceiling of charge over rate
    When a POST request is sent to "/paths/pack/plan/propose" with the body {"siteCode": "WH1", "charge": <charge>, "plannedRate": <rate>}
    Then the last request succeeds with status 200
    And the response field "proposedHeads" is the number <heads>
    And the response field "rateSource" is "caller"

    Examples:
      | charge | rate | heads |
      | 100    | 30   | 4     |
      | 90     | 30   | 3     |
      | 91     | 30   | 4     |
      | 1      | 30   | 1     |
      | 0      | 30   | 0     |

  @bdd
  Scenario: A proposal is announced but never stored
    When a POST request is sent to "/paths/pack/plan/propose" with the body {"siteCode": "WH1", "charge": 100, "plannedRate": 30}
    Then 1 "ShiftPlanProposed" event was published
    And 0 "ShiftPlanCommitted" events were published
    When the staffing gap for path "pack" is requested for building "WH1" shift "shift-1"
    Then the request is rejected with status 404 and problem type "resource-not-found"

  @bdd
  Scenario: The deprecated buildingId names the site too
    When a POST request is sent to "/paths/pack/plan/propose" with the body {"buildingId": "WH1", "charge": 100, "plannedRate": 30}
    Then the last request succeeds with status 200

  @bdd
  Scenario: Conflicting site names are unprocessable
    When a POST request is sent to "/paths/pack/plan/propose" with the body {"siteCode": "WH1", "buildingId": "WH2", "charge": 100, "plannedRate": 30}
    Then the request is rejected with status 422 and problem type "conflicting-site-and-building"
    And 0 "ShiftPlanProposed" events were published

  @bdd
  Scenario: A proposal without any site key is rejected
    When a POST request is sent to "/paths/pack/plan/propose" with the body {"charge": 100, "plannedRate": 30}
    Then the request is rejected with status 400 and problem type "missing-building-id"

  @bdd
  Scenario: An omitted charge is a client mistake
    When a POST request is sent to "/paths/pack/plan/propose" with the body {"siteCode": "WH1", "plannedRate": 30}
    Then the request is rejected with status 400
    And 0 "ShiftPlanProposed" events were published

  @bdd
  Scenario: A malformed proposal body is rejected
    When a POST request is sent to "/paths/pack/plan/propose" with the body {"siteCode": "WH1", "charge":
    Then the request is rejected with status 400 and problem type "malformed-request-body"

  @bdd
  Scenario Outline: A rate that is not supplied yields no heads rather than a guess
    When a POST request is sent to "/paths/pack/plan/propose" with the body <body>
    Then the last request succeeds with status 200
    And the response field "proposedHeads" is the number 0
    And the response field "rateSource" is "caller"

    Examples:
      | body                                              |
      | {"siteCode": "WH1", "charge": 100}                |
      | {"siteCode": "WH1", "charge": 100, "plannedRate": 0}  |
      | {"siteCode": "WH1", "charge": 100, "plannedRate": -5} |

  @bdd
  Scenario Outline: An idle share at or below the threshold does not trim the proposal
    Given the observed idle share for path "pack" is <share>
    When a POST request is sent to "/paths/pack/plan/propose" with the body {"siteCode": "WH1", "charge": 100, "plannedRate": 10}
    Then the response field "proposedHeads" is the number 10
    And the response omits the field "trimReason"

    Examples:
      | share |
      | 0     |
      | 0.2   |
      | 0.3   |

  @bdd
  Scenario: A trim never goes below one head
    Given the observed idle share for path "pack" is 0.95
    When a POST request is sent to "/paths/pack/plan/propose" with the body {"siteCode": "WH1", "charge": 10, "plannedRate": 10}
    Then the last request succeeds with status 200
    And the response field "proposedHeads" is the number 1

  @bdd
  Scenario: Nothing to do is never trimmed into work
    Given the observed idle share for path "pack" is 0.9
    When a POST request is sent to "/paths/pack/plan/propose" with the body {"siteCode": "WH1", "charge": 0, "plannedRate": 10}
    Then the response field "proposedHeads" is the number 0
