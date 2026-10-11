# Associate roster scenarios: starting, certifying and ending a shift.
#
# Derived from:
#   - apis/openapi.yaml, POST /associates/{id}/start-shift: 201, naturally
#     idempotent by the client-supplied id ("calling it twice for the same
#     associate upserts the roster entry rather than erroring (each call
#     still re-raises AssociateShiftStarted and returns 201)"); 400 for a
#     bad body
#   - POST /associates/{id}/certifications: 204; "Adding a certification
#     the associate already holds is a no-op on state (set semantics) but
#     still re-raises the event and returns 204"; 404 for an unknown
#     associate; 409 associate-shift-ended
#   - POST /associates/{id}/end-shift: 204, an idempotent no-op when already
#     ended; 404 for an unknown associate
#   - .claude/rules/domain-model.md: AssociateShift, Certification

Feature: Keeping the associate roster for a shift
  As a shift manager
  I want associates started, certified and ended once and visibly
  So that the labor picture only ever contains people who are really on shift

  @bdd
  Scenario: Starting a shift puts the associate on the roster
    When a POST request is sent to "/associates/assoc-1/start-shift" with the body {"certifications": ["pack"], "siteCode": "WH1"}
    Then the last request succeeds with status 201
    And the response field "associateId" is "assoc-1"
    And 1 "AssociateShiftStarted" event was published

  @bdd
  Scenario: Starting the same associate's shift twice is an upsert, not an error
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    When a POST request is sent to "/associates/assoc-1/start-shift" with the body {"certifications": ["pack", "pick"]}
    Then the last request succeeds with status 201
    And 2 "AssociateShiftStarted" events were published

  @bdd
  Scenario: A malformed start-shift body is rejected
    When a POST request is sent to "/associates/assoc-1/start-shift" with the body {"certifications": [
    Then the request is rejected with status 400 and problem type "malformed-request-body"
    And 0 "AssociateShiftStarted" events were published

  @bdd
  Scenario: A certification makes a path assignable
    Given an AssociateShift is started for associate "assoc-1" with no certifications
    When a POST request is sent to "/associates/assoc-1/certifications" with the body {"certification": "pack"}
    Then the last request succeeds with status 204
    And 1 "AssociateCertified" event was published
    When associate "assoc-1" is assigned to path "pack"
    Then the LaborAssignment is created with active path "pack"

  @bdd
  Scenario: Certifying twice leaves the state alone but still announces it
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    When associate "assoc-1" is certified for "pack"
    Then the last request succeeds with status 204
    And 1 "AssociateCertified" event was published

  @bdd
  Scenario: Certifying an associate who never started a shift is not found
    When a POST request is sent to "/associates/ghost/certifications" with the body {"certification": "pack"}
    Then the request is rejected with status 404 and problem type "resource-not-found"
    And 0 "AssociateCertified" events were published

  @bdd
  Scenario: Certifying an associate whose shift has ended is rejected
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    And associate "assoc-1" has ended their shift
    When a POST request is sent to "/associates/assoc-1/certifications" with the body {"certification": "hazmat"}
    Then the request is rejected with status 409 and problem type "associate-shift-ended"

  @bdd
  Scenario Outline: A certification request that is not valid is rejected
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    When a POST request is sent to "/associates/assoc-1/certifications" with the body <body>
    Then the request is rejected with status 400 and problem type "<problem>"

    Examples:
      | body                  | problem                    |
      | {"certification": ""} | empty-certification        |
      | {}                    | empty-certification        |
      | {"certification":     | malformed-request-body     |

  @bdd
  Scenario: Ending a shift is announced once
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    When associate "assoc-1" ends their shift
    Then the last request succeeds with status 204
    And 1 "AssociateShiftEnded" event was published

  @bdd
  Scenario: Ending a shift twice announces it once
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    And associate "assoc-1" has ended their shift
    When associate "assoc-1" ends their shift
    Then the last request succeeds with status 204
    And 1 "AssociateShiftEnded" event was published

  @bdd
  Scenario: Ending the shift of an associate who never started one is not found
    When associate "ghost" ends their shift
    Then the request is rejected with status 404 and problem type "resource-not-found"
