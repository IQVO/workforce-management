# Documentation-derived rules for the StaffingGap read model and its paging.
#
# Derived from:
#   - use-cases.md, GetStaffingGap: "understaffed = activeHeads < plannedHeads";
#     "observedIdlePct is filled only when an idle-share client is wired.
#     Otherwise it is null, never 0"; "siteCode and the deprecated buildingId
#     must agree (conflicting-site-and-building)"; "Raises PathUnderstaffed
#     (path, planned, active, site) for each understaffed line ... every query
#     of an understaffed path publishes again"
#   - business-context.md: "PathUnderstaffed is a flag, not a decision ... It
#     does NOT pick a victim path, rank associates ... or write a
#     LaborAssignment on anyone's behalf"; the gap is a projection of the plan
#     and the live assignments
#   - domain-events.md: PathUnderstaffed {pathId, plannedHeads, activeHeads,
#     optional siteCode}; "absent = unscoped/fleet-wide"
#   - aggregate-design-canvas.md, AssociateShift: an associate with no site
#     "counts only in unscoped staffing-gap queries" (ADR 0034)
#   - apis/openapi.yaml + ADR 0040: collection GETs are keyset-paged
#     {items,nextCursor} ordered by pathId, limit 1-500 (empty or repeated is
#     400 invalid-limit), undecodable or repeated cursor is 400 invalid-cursor,
#     any other undeclared query parameter is 400 unknown-query-parameter;
#     the deprecated route carries `Deprecation: true` on EVERY response
#   - troubleshooting.md: 400 unknown-path-id
#   - NOT pinned (docs contradict each other): troubleshooting.md and
#     use-cases.md say siteCode vs buildingId disagreeing on a staffing-gap read
#     is 422 conflicting-site-and-building; ADR 0035 "Open item" says the rule is
#     deliberately NOT applied to gap reads yet, and staffing_gap_views.feature
#     already pins siteCode as scope + buildingId as plan key.

Feature: The staffing gap is a flag raised from the plan and the live assignments
  As a shift manager
  I want the gap to be exactly planned versus active, announced every time, and never acted on
  So that I decide who moves, with an honest picture of where we are short

  @bdd
  Scenario Outline: A path is understaffed only while active heads are below the plan
    Given a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 2            | 30          | 16           | 5                 |
    And <active> associates certified for "pack" are working path "pack"
    When a GET request is sent to "/paths/pack/staffing-gap?buildingId=WH1&shiftId=shift-1"
    Then the last request succeeds with status 200
    And the response field "plannedHeads" is the number 2
    And the response field "activeHeads" is the number <active>
    And the response field "understaffed" is <flag>
    And <events> "PathUnderstaffed" events were published

    Examples:
      | active | flag  | events |
      | 0      | true  | 1      |
      | 1      | true  | 1      |
      | 2      | false | 0      |
      | 3      | false | 0      |

  @bdd
  Scenario: Every query of an understaffed path announces the gap again
    Given a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 2            | 30          | 16           | 5                 |
    When a GET request is sent to "/paths/pack/staffing-gap?buildingId=WH1&shiftId=shift-1"
    And a GET request is sent to "/paths/pack/staffing-gap?buildingId=WH1&shiftId=shift-1"
    Then 2 "PathUnderstaffed" events were published
    And the last PathUnderstaffed event is for path "pack" with 2 planned and 0 active heads across all sites

  @bdd
  Scenario: Surfacing a gap never moves anyone
    Given an AssociateShift is started for associate "assoc-1" with certifications "pick,pack"
    And a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 1            | 30          | 8            | 5                 |
      | pick   | 1            | 30          | 8            | 5                 |
    And associate "assoc-1" is assigned to path "pack"
    When a GET request is sent to "/buildings/WH1/shifts/shift-1/staffing-gap"
    Then 1 "PathUnderstaffed" events were published
    And the last PathUnderstaffed event is for path "pick" with 1 planned and 0 active heads across all sites
    And 1 "LaborAssigned" event was published
    And 0 "LaborReassigned" events were published
    And path "pack" has 1 active head in building "WH1" shift "shift-1"
    And path "pick" has 0 active heads in building "WH1" shift "shift-1"

  @bdd
  Scenario: A gap scoped to a site names that site in the event
    Given an AssociateShift is started for associate "assoc-1" at site "WH1" with certifications "pack"
    And a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 3            | 30          | 24           | 5                 |
    And associate "assoc-1" is assigned to path "pack"
    When a GET request is sent to "/sites/WH1/shifts/shift-1/staffing-gap"
    Then the last PathUnderstaffed event is for path "pack" with 3 planned and 1 active heads at site "WH1"

  @bdd
  Scenario: An unscoped gap leaves the site out of the event
    Given an AssociateShift is started for associate "assoc-1" at site "WH1" with certifications "pack"
    And a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 3            | 30          | 24           | 5                 |
    And associate "assoc-1" is assigned to path "pack"
    When a GET request is sent to "/buildings/WH1/shifts/shift-1/staffing-gap"
    Then the last PathUnderstaffed event is for path "pack" with 3 planned and 1 active heads across all sites

  @bdd
  Scenario: An associate with no site counts only in the unscoped gap
    Given an AssociateShift is started for associate "assoc-1" with certifications "pack"
    And a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 1            | 30          | 8            | 5                 |
    And associate "assoc-1" is assigned to path "pack"
    When a GET request is sent to "/sites/WH1/shifts/shift-1/staffing-gap"
    Then the response field "items.0.activeHeads" is the number 0
    And the response field "items.0.understaffed" is true
    When a GET request is sent to "/buildings/WH1/shifts/shift-1/staffing-gap"
    Then the response field "items.0.activeHeads" is the number 1
    And the response field "items.0.understaffed" is false

  @bdd
  Scenario: The deprecated building route accepts the optional site scope
    Given an AssociateShift is started for associate "assoc-1" at site "WH1" with certifications "pack"
    And an AssociateShift is started for associate "assoc-2" at site "WH2" with certifications "pack"
    And a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 3            | 30          | 24           | 5                 |
    And associate "assoc-1" is assigned to path "pack"
    And associate "assoc-2" is assigned to path "pack"
    When a GET request is sent to "/buildings/WH1/shifts/shift-1/staffing-gap?siteCode=WH2"
    Then the response field "items.0.activeHeads" is the number 1

  @bdd
  Scenario: A gap read by the deprecated building key alone is not scoped to a site
    Given a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 1            | 30          | 8            | 5                 |
    When a GET request is sent to "/paths/pack/staffing-gap?buildingId=WH1&shiftId=shift-1"
    Then the last request succeeds with status 200
    And the response omits the field "siteCode"

  @bdd
  Scenario: A path's gap without a shift id is rejected
    When a GET request is sent to "/paths/pack/staffing-gap?siteCode=WH1"
    Then the request is rejected with status 400 and problem type "missing-shift-id"

  @bdd
  Scenario: A path the catalogue does not declare has no gap to read
    When a GET request is sent to "/paths/teleport/staffing-gap?siteCode=WH1&shiftId=shift-1"
    Then the request is rejected with status 400 and problem type "unknown-path-id"

  @bdd
  Scenario: The deprecated route says so even when it answers an error
    When a GET request is sent to "/buildings/WH9/shifts/shift-1/staffing-gap"
    Then the request is rejected with status 404 and problem type "resource-not-found"
    And the response header "Deprecation" is "true"

  @bdd
  Scenario: A measured idle share is surfaced as a fraction under both field names
    Given the observed idle share for path "pack" is 0.4
    And a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 2            | 30          | 16           | 5                 |
    When a GET request is sent to "/paths/pack/staffing-gap?buildingId=WH1&shiftId=shift-1"
    Then the response field "observedIdleShare" is the number 0.4
    And the response fields "observedIdleShare" and "observedIdlePct" hold the same value
    And the response field "understaffed" is true

  @bdd
  Scenario: Without an idle observation the gap carries no idle share, never zero
    Given no idle share has been observed for path "pack"
    And a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 2            | 30          | 16           | 5                 |
    When a GET request is sent to "/paths/pack/staffing-gap?buildingId=WH1&shiftId=shift-1"
    Then the last request succeeds with status 200
    And the response omits the field "observedIdleShare"
    And the response omits the field "observedIdlePct"

  @bdd
  Scenario: Without an idle-share signal wired the gap carries no idle share either
    Given a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 2            | 30          | 16           | 5                 |
    When a GET request is sent to "/paths/pack/staffing-gap?buildingId=WH1&shiftId=shift-1"
    Then the response omits the field "observedIdleShare"
    And the response omits the field "observedIdlePct"

  @bdd
  Scenario: A page that holds every planned path has no next cursor
    Given a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 2            | 30          | 16           | 5                 |
      | pick   | 1            | 30          | 8            | 5                 |
    When a GET request is sent to "/sites/WH1/shifts/shift-1/staffing-gap?limit=2"
    Then the response is a list of 2 entries
    And the response omits the field "nextCursor"

  @bdd
  Scenario: Paths are paged in pathId order and the last page carries no cursor
    Given a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pick   | 1            | 30          | 8            | 5                 |
      | pack   | 2            | 30          | 16           | 5                 |
    When a GET request is sent to "/sites/WH1/shifts/shift-1/staffing-gap?limit=1"
    Then the response is a list of 1 entries
    And the response field "items.0.pathId" is "pack"
    And the response carries a nextCursor
    When the next page of "/sites/WH1/shifts/shift-1/staffing-gap?limit=1" is requested with the returned cursor
    Then the response is a list of 1 entries
    And the response field "items.0.pathId" is "pick"
    And the response omits the field "nextCursor"

  @bdd
  Scenario Outline: A page size from one to five hundred is accepted
    Given a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 2            | 30          | 16           | 5                 |
    When a GET request is sent to "/sites/WH1/shifts/shift-1/staffing-gap?limit=<limit>"
    Then the last request succeeds with status 200

    Examples:
      | limit |
      | 1     |
      | 500   |

  @bdd
  Scenario Outline: A page size that is not a single integer from one to five hundred is rejected
    When a GET request is sent to "/sites/WH1/shifts/shift-1/staffing-gap?limit=<limit>"
    Then the request is rejected with status 400 and problem type "invalid-limit"

    Examples:
      | limit     |
      | 0         |
      | 501       |
      | -1        |
      | abc       |
      |           |
      | 1&limit=2 |

  @bdd
  Scenario Outline: A cursor that is undecodable or repeated is rejected
    When a GET request is sent to "/sites/WH1/shifts/shift-1/staffing-gap?<query>"
    Then the request is rejected with status 400 and problem type "invalid-cursor"

    Examples:
      | query                |
      | cursor=not-a-cursor  |
      | cursor=a&cursor=b    |

  @bdd
  Scenario Outline: A query parameter the collection does not declare is rejected
    When a GET request is sent to "/sites/WH1/shifts/shift-1/staffing-gap?<query>"
    Then the request is rejected with status 400 and problem type "unknown-query-parameter"

    Examples:
      | query    |
      | offset=1 |
      | after=x  |
      | page=2   |
