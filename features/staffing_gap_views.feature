# Staffing-gap read model: the all-paths lists, the site scope and the
# deprecated alias.
#
# Derived from:
#   - apis/openapi.yaml, GET /sites/{siteCode}/shifts/{shiftId}/staffing-gap
#     (canonical): "List the staffing gap for every path planned within one
#     committed shift plan of a site ... every path's activeHeads counts only
#     associates with an active shift at that site (ADR 0034) ... raises
#     PathUnderstaffed for every understaffed path in the plan"; 404
#     resource-not-found without a committed plan
#   - GET /buildings/{buildingId}/shifts/{shiftId}/staffing-gap: DEPRECATED
#     alias; "Every response carries the header Deprecation: true"; a
#     buildingId-only call is NOT scoped by site
#   - GET /paths/{pathId}/staffing-gap: siteCode or the deprecated buildingId
#     is required (neither is 400 missing-building-id); shiftId is required;
#     "Accepted as given, never validated against facility-layout, so an
#     unknown code simply counts 0"
#   - docs/docs/adr/0034 and 0035

Feature: Reading the staffing gap
  As a shift manager
  I want to see every path's planned versus active heads for a site and shift
  So that I know where to move people, without the system moving them for me

  Background:
    Given an AssociateShift is started for associate "assoc-1" at site "WH1" with certifications "pack"
    And an AssociateShift is started for associate "assoc-2" at site "WH2" with certifications "pack"
    And a ShiftPlan is committed for building "WH1" shift "shift-1" with lines:
      | pathId | plannedHeads | plannedRate | plannedHours | installedStations |
      | pack   | 3            | 30          | 8            | 10                |
      | pick   | 1            | 30          | 8            | 10                |

  @bdd
  Scenario: The canonical site route lists every planned path
    Given associate "assoc-1" is assigned to path "pack"
    When a GET request is sent to "/sites/WH1/shifts/shift-1/staffing-gap"
    Then the last request succeeds with status 200
    And the response is a list of 2 entries
    And the response field "0.pathId" is "pack"
    And the response field "0.plannedHeads" is the number 3
    And the response field "0.activeHeads" is the number 1
    And the response field "0.understaffed" is true
    And the response field "1.pathId" is "pick"
    And the response field "1.activeHeads" is the number 0
    And the response field "1.understaffed" is true

  @bdd
  Scenario: A site's list counts only associates active at that site
    Given associate "assoc-1" is assigned to path "pack"
    And associate "assoc-2" is assigned to path "pack"
    When a GET request is sent to "/sites/WH1/shifts/shift-1/staffing-gap"
    Then the response field "0.activeHeads" is the number 1

  @bdd
  Scenario: Every understaffed path in the plan is announced
    When a GET request is sent to "/sites/WH1/shifts/shift-1/staffing-gap"
    Then 2 "PathUnderstaffed" events were published

  @bdd
  Scenario: A fully staffed plan announces nothing
    Given an AssociateShift is started for associate "assoc-3" at site "WH1" with certifications "pick"
    And associate "assoc-3" is assigned to path "pick"
    When a GET request is sent to "/paths/pick/staffing-gap?siteCode=WH1&shiftId=shift-1"
    Then the last request succeeds with status 200
    And the response field "understaffed" is false
    And 0 "PathUnderstaffed" events were published

  @bdd
  Scenario: The deprecated building route works and says it is deprecated
    When a GET request is sent to "/buildings/WH1/shifts/shift-1/staffing-gap"
    Then the last request succeeds with status 200
    And the response is a list of 2 entries
    And the response header "Deprecation" is "true"

  @bdd
  Scenario: The canonical site route is not marked deprecated
    When a GET request is sent to "/sites/WH1/shifts/shift-1/staffing-gap"
    Then the response has no header "Deprecation"

  @bdd
  Scenario: The deprecated building route is not scoped by site
    Given associate "assoc-1" is assigned to path "pack"
    And associate "assoc-2" is assigned to path "pack"
    When a GET request is sent to "/buildings/WH1/shifts/shift-1/staffing-gap"
    Then the response field "0.activeHeads" is the number 2

  @bdd
  Scenario Outline: The lists are not found without a committed plan
    When a GET request is sent to "<route>"
    Then the request is rejected with status 404 and problem type "resource-not-found"

    Examples:
      | route                                     |
      | /sites/WH9/shifts/shift-1/staffing-gap    |
      | /sites/WH1/shifts/shift-9/staffing-gap    |
      | /buildings/WH9/shifts/shift-1/staffing-gap |

  @bdd
  Scenario: A path's gap can be read by the canonical siteCode alone
    When a GET request is sent to "/paths/pack/staffing-gap?siteCode=WH1&shiftId=shift-1"
    Then the last request succeeds with status 200
    And the response field "siteCode" is "WH1"
    And the response field "plannedHeads" is the number 3

  @bdd
  Scenario: A path's gap without any site key is rejected
    When a GET request is sent to "/paths/pack/staffing-gap?shiftId=shift-1"
    Then the request is rejected with status 400 and problem type "missing-building-id"

  @bdd
  Scenario: A path's gap for a site with no associates counts zero active heads
    Given associate "assoc-1" is assigned to path "pack"
    When a GET request is sent to "/paths/pack/staffing-gap?siteCode=WH1&buildingId=WH1&shiftId=shift-1"
    Then the response field "activeHeads" is the number 1
    When a GET request is sent to "/paths/pack/staffing-gap?siteCode=WH9&buildingId=WH1&shiftId=shift-1"
    Then the response field "activeHeads" is the number 0
