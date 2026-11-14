# Documentation-derived rules for AssignLabor and EndAssociateShift.
#
# Derived from:
#   - use-cases.md, AssignLabor "Invariants, in order": (1) the associate
#     exists, (2) CanBeAssigned: shift not ended / not on break, (3) the
#     certification gate: the certification named after the path's catalogue
#     family (MatchPrefix, so PICK, pick and pick-zone-a all require pick),
#     (4) single active assignment (closed and moved to history, never
#     rejected), (5) the closed interval's hours must fit under
#     MAX_HOURS_PER_SHIFT (ErrMaxHoursExceeded); raises LaborAssigned, or
#     LaborReassigned (from path -> to path) when an active assignment was closed
#   - use-cases.md, EndAssociateShift: the active assignment is ended first and
#     its hours logged against the shift; "Closing the assignment raises no
#     assignment event"; logging must not exceed MAX_HOURS_PER_SHIFT
#   - aggregate-design-canvas.md, LaborAssignment state diagram
#     (Unassigned -> Active -> Active / Unassigned) and the corrective policy
#     "if that would exceed MAX_HOURS_PER_SHIFT, the whole assignment is rejected"
#   - business-context.md, "Why it stops at the path boundary": there is no
#     taskId anywhere in this codebase
#   - domain-events.md: LaborAssigned {associateId, pathId},
#     LaborReassigned {associateId, fromPathId, toPathId}
# The harness runs with MAX_HOURS_PER_SHIFT = 8 and a movable clock.

Feature: Assigning labor follows the documented invariants in order
  As a shift manager
  I want every assignment gated by the documented rules, in the documented order
  So that rebalancing never double-books, over-works or mislabels an associate

  @bdd
  Scenario Outline: A certification covers every path of its catalogue family
    Given an AssociateShift is started for associate "assoc-1" with certifications "<certification>"
    When associate "assoc-1" is assigned to path "<path>"
    Then the last request succeeds with status 201
    And the response field "activePathId" is "<path>"

    Examples:
      | certification | path           |
      | pick          | PICK           |
      | pick          | pick           |
      | pick          | pick-zone-a    |
      | rebin         | REBIN          |
      | slam          | slam-lane-2    |
      | pack          | Pack-Station-7 |

  @bdd
  Scenario Outline: A certification of another family, or a zone name, is not the family's certification
    Given an AssociateShift is started for associate "assoc-1" with certifications "<certification>"
    When associate "assoc-1" is assigned to path "<path>"
    Then the request is rejected with status 409 and problem type "certification-required"

    Examples:
      | certification | path        |
      | pack          | PICK        |
      | pack          | pick-zone-a |
      | pick          | PACK        |
      | pick          | slam-lane-2 |
      | pick-zone-a   | pick-zone-a |

  @bdd
  Scenario: The shift state is checked before the certification (ended shift)
    Given an AssociateShift is started for associate "assoc-1" with certifications "pick"
    And associate "assoc-1" has ended their shift
    When associate "assoc-1" is assigned to path "pack"
    Then the request is rejected with status 409 and problem type "associate-shift-ended"

  @bdd
  Scenario: The shift state is checked before the certification (on break)
    Given an AssociateShift is started for associate "assoc-1" with certifications "pick"
    And associate "assoc-1" has started a break
    When associate "assoc-1" is assigned to path "pack"
    Then the request is rejected with status 409 and problem type "associate-on-break"

  @bdd
  Scenario Outline: Closing an interval of up to the shift cap is accepted
    Given an AssociateShift is started for associate "assoc-1" with certifications "pick,pack"
    And associate "assoc-1" is assigned to path "pick"
    And the clock advances by <worked> hours
    When associate "assoc-1" is assigned to path "pack"
    Then the last request succeeds with status 201
    And 1 "LaborReassigned" event was published

    Examples:
      | worked |
      | 1      |
      | 7.5    |
      | 8      |

  @bdd
  Scenario Outline: Closing an interval beyond the shift cap rejects the whole assignment
    Given an AssociateShift is started for associate "assoc-1" with certifications "pick,pack"
    And a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pick   | 1            | 30          | 8            | 5                 |
      | pack   | 1            | 30          | 8            | 5                 |
    And associate "assoc-1" is assigned to path "pick"
    And the clock advances by <worked> hours
    When associate "assoc-1" is assigned to path "pack"
    Then the request is rejected with status 409 and problem type "max-hours-exceeded"
    And 0 "LaborReassigned" events were published
    And path "pick" has 1 active head in building "WH1" shift "shift-1"
    And path "pack" has 0 active heads in building "WH1" shift "shift-1"

    Examples:
      | worked |
      | 8.5    |
      | 12     |

  @bdd
  Scenario Outline: Hours add up across intervals against the same cap
    Given an AssociateShift is started for associate "assoc-1" with certifications "pick,pack"
    And associate "assoc-1" is assigned to path "pick"
    And the clock advances by 5 hours
    And associate "assoc-1" is assigned to path "pack"
    And the clock advances by <second> hours
    When associate "assoc-1" is assigned to path "pick"
    Then the response status is <status>

    Examples:
      | second | status |
      | 3      | 201    |
      | 3.5    | 409    |

  @bdd
  Scenario Outline: Ending a shift logs the closing interval against the same cap
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    And associate "assoc-1" is assigned to path "pack"
    And the clock advances by <worked> hours
    When associate "assoc-1" ends their shift
    Then the response status is <status>

    Examples:
      | worked | status |
      | 8      | 204    |
      | 8.5    | 409    |

  @bdd
  Scenario: A shift whose closing interval is over the cap does not end
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    And a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 1            | 30          | 8            | 5                 |
    And associate "assoc-1" is assigned to path "pack"
    And the clock advances by 9 hours
    When associate "assoc-1" ends their shift
    Then the request is rejected with status 409 and problem type "max-hours-exceeded"
    And 0 "AssociateShiftEnded" events were published
    And path "pack" has 1 active head in building "WH1" shift "shift-1"

  @bdd
  Scenario: Ending a shift closes the assignment without raising an assignment event
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    And associate "assoc-1" is assigned to path "pack"
    When associate "assoc-1" ends their shift
    Then the last request succeeds with status 204
    And 1 "AssociateShiftEnded" event was published
    And 1 "LaborAssigned" event was published
    And 0 "LaborReassigned" events were published

  @bdd
  Scenario: After the shift ended and restarted, the next assignment is a first assignment again
    Given an AssociateShift is started for associate "assoc-1" with certifications "pick"
    And associate "assoc-1" is assigned to path "pick"
    And associate "assoc-1" has ended their shift
    And associate "assoc-1" restarts their shift with certifications "pick"
    When associate "assoc-1" is assigned to path "pick"
    Then the last request succeeds with status 201
    And 2 "LaborAssigned" events were published
    And 0 "LaborReassigned" events were published

  @bdd
  Scenario: A first assignment names the associate and the path as asked
    Given an AssociateShift is started for associate "assoc-1" with certifications "pick"
    When associate "assoc-1" is assigned to path "PICK"
    Then the last LaborAssigned event placed associate "assoc-1" on path "PICK"

  @bdd
  Scenario: Each move names the path it leaves and the path it joins
    Given an AssociateShift is started for associate "assoc-1" with certifications "pick,pack,rebin"
    And associate "assoc-1" is assigned to path "pick"
    And associate "assoc-1" is assigned to path "pack"
    And associate "assoc-1" is assigned to path "rebin"
    Then 1 "LaborAssigned" event was published
    And 2 "LaborReassigned" events were published
    And the last LaborReassigned event moved associate "assoc-1" from path "pack" to path "rebin"

  @bdd
  Scenario: An assignment ends at the path: no task is ever linked to the associate
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    When a POST request is sent to "/associates/assoc-1/assignments" with the body {"pathId": "pack", "taskId": "TASK-1"}
    Then the last request succeeds with status 201
    And the response field "activePathId" is "pack"
    And the response omits the field "taskId"

  @bdd
  Scenario Outline: An assignment request without a usable path names the empty path id
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    When a POST request is sent to "/associates/assoc-1/assignments" with the body <body>
    Then the request is rejected with status 400 and problem type "<problem>"
    And 0 "LaborAssigned" events were published

    Examples:
      | body           | problem                |
      | {"pathId": ""} | empty-path-id          |
      | {}             | empty-path-id          |
      | {"pathId":     | malformed-request-body |
