# Derived from apis/openapi.yaml:
# - POST /associates/{id}/break/start 409: "the associate is already on
#   break (associate-already-on-break)" — "Not idempotent: calling this
#   while already on break is rejected as a conflict rather than treated
#   as a no-op, since it represents a discrete state transition".
# - POST /associates/{id}/break/end 409: "the associate is not currently
#   on break (associate-not-on-break)" — same non-idempotent discipline.
Feature: Break state transitions are discrete, not upserts
  As a shift manager
  I want break start and end to be distinct state transitions
  So that a double-recorded or fabricated break never distorts the shift's hours

  @bdd
  Scenario: Starting a break while already on break is rejected
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    And associate "assoc-1" has started a break
    When associate "assoc-1" starts a break
    Then the request is rejected with status 409 and problem type "associate-already-on-break"

  @bdd
  Scenario: Ending a break when the associate is not on break is rejected
    Given an AssociateShift is started for associate "assoc-2" with certifications "pack"
    When associate "assoc-2" ends the break
    Then the request is rejected with status 409 and problem type "associate-not-on-break"
