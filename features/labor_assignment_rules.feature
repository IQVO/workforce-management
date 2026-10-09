# Assignment events and validation.
#
# Derived from:
#   - apis/openapi.yaml, POST /associates/{id}/assignments: "Puts an
#     associate on pathId for a new interval, raising LaborAssigned (first
#     assignment) or LaborReassigned (an assignment was already active)";
#     201 with associateId, activePathId and active; 400 for a bad body or a
#     pathId the process-path catalogue does not declare; 404 for an unknown
#     associate
#   - .claude/rules/domain-model.md: exactly one ACTIVE assignment per
#     associate, by construction

Feature: Assigning labor to paths
  As a shift manager
  I want every assignment and every move to be announced for what it is
  So that downstream consumers can tell a first assignment from a rebalance

  @bdd
  Scenario: A first assignment is announced as an assignment
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    When associate "assoc-1" is assigned to path "pack"
    Then the last request succeeds with status 201
    And the response field "associateId" is "assoc-1"
    And the response field "activePathId" is "pack"
    And the response field "active" is true
    And 1 "LaborAssigned" event was published
    And 0 "LaborReassigned" events were published

  @bdd
  Scenario: Moving an associate is announced as a reassignment
    Given an AssociateShift is started for associate "assoc-1" with certifications "pick,pack"
    And associate "assoc-1" is assigned to path "pick"
    When associate "assoc-1" is assigned to path "pack"
    Then the last request succeeds with status 201
    And the response field "activePathId" is "pack"
    And 1 "LaborAssigned" event was published
    And 1 "LaborReassigned" event was published

  @bdd
  Scenario: Assigning an associate who never started a shift is not found
    When associate "ghost" is assigned to path "pack"
    Then the request is rejected with status 404 and problem type "resource-not-found"
    And 0 "LaborAssigned" events were published

  @bdd
  Scenario: Assigning to a path the catalogue does not declare is rejected
    Given an AssociateShift is started for associate "assoc-1" with certifications "teleport"
    When associate "assoc-1" is assigned to path "teleport"
    Then the request is rejected with status 400 and problem type "unknown-path-id"
    And 0 "LaborAssigned" events were published

  @bdd
  Scenario Outline: An assignment request that is not valid is rejected
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    When a POST request is sent to "/associates/assoc-1/assignments" with the body <body>
    Then the request is rejected with status 400

    Examples:
      | body             |
      | {"pathId": ""}   |
      | {}               |
      | {"pathId":       |

  @bdd
  Scenario: A rejected assignment leaves the previous one in place
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    And a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 1            | 30          | 8            | 5                 |
      | pick   | 1            | 30          | 8            | 5                 |
    And associate "assoc-1" is assigned to path "pack"
    When associate "assoc-1" is assigned to path "pick"
    Then the request is rejected with status 409 and problem type "certification-required"
    And path "pack" has 1 active head in building "WH1" shift "shift-1"
    And path "pick" has 0 active heads in building "WH1" shift "shift-1"
