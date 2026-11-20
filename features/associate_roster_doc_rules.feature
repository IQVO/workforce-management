# Documentation-derived rules for the AssociateShift roster entry.
#
# Derived from (warehouse-docs docs/contexts/workforce-management and the
# in-repo docs/docs/ddd copies):
#   - use-cases.md, StartAssociateShift: "idempotent by associate id. A second
#     call REPLACES the roster entry (certifications and site)"; invariants:
#     non-empty associate id
#   - aggregate-design-canvas.md, AssociateShift state diagram:
#       OnBreak --> OnBreak: Certify; OnBreak --> Ended: EndShift;
#       Ended --> OnShift: StartAssociateShift replaces the roster entry
#     corrective policy: "Restarting replaces the entry -- site included"
#   - use-cases.md, EndAssociateShift / AssignLabor (hours logged against the
#     shift, MAX_HOURS_PER_SHIFT), domain-events.md (payloads)
#   - aggregate-design-canvas.md invariants: "Non-empty identity and
#     certification values" (shared.ErrEmptyCertification)
# The scenarios already in associate_roster.feature / break_*.feature pin the
# plain transitions; this file pins the replace-on-restart rules and the edges
# of the state diagram that nothing else drives.

Feature: The associate roster entry is replaced on restart and follows its state diagram
  As a shift manager
  I want a restarted shift to be a clean roster entry and every state edge to behave as documented
  So that what an associate held before a restart never leaks into the new shift

  @bdd
  Scenario: Restarting a shift replaces the certifications instead of merging them
    Given an AssociateShift is started for associate "assoc-1" with certifications "pick"
    And associate "assoc-1" restarts their shift with certifications "pack"
    When associate "assoc-1" is assigned to path "pick"
    Then the request is rejected with status 409 and problem type "certification-required"
    When associate "assoc-1" is assigned to path "pack"
    Then the LaborAssignment is created with active path "pack"

  @bdd
  Scenario: Restarting a shift replaces the site
    Given an AssociateShift is started for associate "assoc-1" at site "WH1" with certifications "pack"
    And a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 2            | 30          | 16           | 5                 |
    And associate "assoc-1" is assigned to path "pack"
    And associate "assoc-1" restarts their shift at site "WH2" with certifications "pack"
    When the staffing gap for path "pack" is requested for building "WH1" shift "shift-1" at site "WH1"
    Then the response field "activeHeads" is the number 0
    When the staffing gap for path "pack" is requested for building "WH1" shift "shift-1" at site "WH2"
    Then the response field "activeHeads" is the number 1

  @bdd
  Scenario: Restarting a shift without a site leaves the associate site-less
    Given an AssociateShift is started for associate "assoc-1" at site "WH1" with certifications "pack"
    And a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 2            | 30          | 16           | 5                 |
    And associate "assoc-1" is assigned to path "pack"
    And associate "assoc-1" restarts their shift with certifications "pack"
    When the staffing gap for path "pack" is requested for building "WH1" shift "shift-1" at site "WH1"
    Then the response field "activeHeads" is the number 0
    When the staffing gap for path "pack" is requested for building "WH1" shift "shift-1"
    Then the response field "activeHeads" is the number 1

  @bdd
  Scenario: A restart puts an associate whose shift ended back on shift
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    And associate "assoc-1" has ended their shift
    When associate "assoc-1" restarts their shift with certifications "pack"
    And associate "assoc-1" is assigned to path "pack"
    Then the LaborAssignment is created with active path "pack"
    And 2 "AssociateShiftStarted" events were published
    And 1 "AssociateShiftEnded" event was published

  @bdd
  Scenario: A restart clears a break in progress
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    And associate "assoc-1" has started a break
    And associate "assoc-1" restarts their shift with certifications "pack"
    When associate "assoc-1" ends the break
    Then the request is rejected with status 409 and problem type "associate-not-on-break"
    When associate "assoc-1" is assigned to path "pack"
    Then the LaborAssignment is created with active path "pack"

  @bdd
  Scenario: A restart resets the hours logged against the shift
    Given an AssociateShift is started for associate "assoc-1" with certifications "pick,pack"
    And associate "assoc-1" is assigned to path "pick"
    And the clock advances by 6 hours
    And associate "assoc-1" is assigned to path "pack"
    And associate "assoc-1" restarts their shift with certifications "pick,pack"
    And the clock advances by 6 hours
    When associate "assoc-1" is assigned to path "pick"
    Then the last request succeeds with status 201

  @bdd
  Scenario: An associate on break can still be certified, and uses it after the break
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    And associate "assoc-1" has started a break
    When associate "assoc-1" is certified for "pick"
    Then the last request succeeds with status 204
    And the last AssociateCertified event is for associate "assoc-1" with certification "pick"
    When associate "assoc-1" ends the break
    And associate "assoc-1" is assigned to path "pick"
    Then the LaborAssignment is created with active path "pick"

  @bdd
  Scenario: A shift can end while the associate is on break
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    And associate "assoc-1" has started a break
    When associate "assoc-1" ends their shift
    Then the last request succeeds with status 204
    And 1 "AssociateShiftEnded" event was published
    When associate "assoc-1" ends the break
    Then the request is rejected with status 409 and problem type "associate-shift-ended"

  @bdd
  Scenario: A blank certification in the start list is refused before anything is recorded
    When a POST request is sent to "/associates/assoc-1/shift-starts" with the body {"certifications": ["pack", ""]}
    Then the request is rejected with status 400 and problem type "empty-certification"
    And 0 "AssociateShiftStarted" events were published

  @bdd
  Scenario: The shift-started event carries the associate and the certifications they were started with
    When a POST request is sent to "/associates/assoc-1/shift-starts" with the body {"certifications": ["pick", "pack"], "siteCode": "WH1"}
    Then the last request succeeds with status 201
    And the last AssociateShiftStarted event is for associate "assoc-1" with certifications "pick,pack"
