Feature: Committing a ShiftPlan
  As a shift manager
  I want to commit the split of headcount across paths for a shift
  So that the building has one agreed ShiftPlan for the shift

  The ShiftPlan is committed by a human: the software proposes heads, a human
  commits them. Committing independently re-validates the invariant that a
  path's plannedHeads never exceeds its installed stations, and checks it
  against the LIVE station count fulfillment-execution reports for the
  capability the path requires (resolved through the process-path
  catalogue: path "PICK" requires capability "pick").

  @bdd
  Scenario: Committing a ShiftPlan within installed-station capacity succeeds
    When committing a ShiftPlan for building "bldg-1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 6            | 30          | 40           | 10                |
    Then the ShiftPlan commit succeeds with 6 planned heads on path "pack"

  @bdd
  Scenario: Committing a ShiftPlan that exceeds installed stations is rejected
    When committing a ShiftPlan for building "bldg-1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 6            | 30          | 24           | 4                 |
    Then the ShiftPlan commit is rejected with status 409 and problem type "planned-heads-exceed-installed"

  @bdd
  Scenario: A canonical path id is checked against the stations holding its required capability
    Given fulfillment-execution has 41 stations registered with capability "pick"
    When committing a ShiftPlan for building "bldg-1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | PICK   | 4            | 30          | 32           | 10                |
    Then the ShiftPlan commit succeeds with 4 planned heads on path "PICK"

  @bdd
  Scenario: Planned heads above the stations holding the path's capability are rejected
    Given fulfillment-execution has 3 stations registered with capability "pack"
    When committing a ShiftPlan for building "bldg-1" shift "shift-1" with lines:
      | pathId         | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack-station-3 | 4            | 30          | 32           | 10                |
    Then the ShiftPlan commit is rejected with status 409 and problem type "exceeds-installed-capacity"

  @bdd
  Scenario: Committing a ShiftPlan for a path the catalogue does not declare is rejected
    When committing a ShiftPlan for building "bldg-1" shift "shift-1" with lines:
      | pathId          | plannedHeads | plannedRate | plannedHours | installedStations |
      | not-a-real-path | 1            | 30          | 8            | 10                |
    Then the ShiftPlan commit is rejected with status 400 and problem type "unknown-path-id"
