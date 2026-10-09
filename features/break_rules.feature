# Break state-machine edges and events.
#
# Derived from:
#   - apis/openapi.yaml, POST /associates/{id}/break/start: 204; "Not
#     idempotent: calling this while already on break is rejected as a
#     conflict" (associate-already-on-break); 404 unknown associate; 409
#     associate-shift-ended
#   - POST /associates/{id}/break/end: 204; "calling this while not on break
#     is rejected as a conflict" (associate-not-on-break); 404; 409
#     associate-shift-ended

Feature: Logging breaks
  As a shift manager
  I want a break to be a discrete, well-ordered state change
  So that nobody is assigned while away and nobody ends a break that never started

  @bdd
  Scenario: A break is started and ended once each, and announced
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    When associate "assoc-1" starts a break
    Then the last request succeeds with status 204
    And 1 "AssociateBreakStarted" event was published
    When associate "assoc-1" ends the break
    Then the last request succeeds with status 204
    And 1 "AssociateBreakEnded" event was published

  @bdd
  Scenario: Starting a second break while on one is a conflict
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    And associate "assoc-1" has started a break
    When associate "assoc-1" starts a break
    Then the request is rejected with status 409 and problem type "associate-already-on-break"
    And 1 "AssociateBreakStarted" event was published

  @bdd
  Scenario: Ending a break that never started is a conflict
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    When associate "assoc-1" ends the break
    Then the request is rejected with status 409 and problem type "associate-not-on-break"
    And 0 "AssociateBreakEnded" events were published

  @bdd
  Scenario: A break cannot be ended twice
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    And associate "assoc-1" has started a break
    And associate "assoc-1" ends the break
    When associate "assoc-1" ends the break
    Then the request is rejected with status 409 and problem type "associate-not-on-break"

  @bdd
  Scenario: An associate who never started a shift cannot start a break
    When associate "ghost" starts a break
    Then the request is rejected with status 404 and problem type "resource-not-found"

  @bdd
  Scenario: An associate who never started a shift cannot end a break
    When associate "ghost" ends the break
    Then the request is rejected with status 404 and problem type "resource-not-found"

  @bdd
  Scenario: A break cannot be started after the shift has ended
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    And associate "assoc-1" has ended their shift
    When associate "assoc-1" starts a break
    Then the request is rejected with status 409 and problem type "associate-shift-ended"

  @bdd
  Scenario: A break cannot be ended after the shift has ended
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    And associate "assoc-1" has ended their shift
    When associate "assoc-1" ends the break
    Then the request is rejected with status 409 and problem type "associate-shift-ended"
