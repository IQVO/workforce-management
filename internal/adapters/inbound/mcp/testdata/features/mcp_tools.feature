# MCP behavioral evals for the workforce-management tool surface.
#
# Each scenario drives tools/call over the REAL Streamable HTTP handler
# with a connected SDK client — exactly the call a model host makes — and
# pins the structured result and the state side effects. Arguments are
# deliberately model-realistic: extra keys, wrong types, unknown ids.
#
# Derived from the tool contracts documented in:
#   - internal/adapters/inbound/mcp/tools.go and report_tool.go (tool
#     descriptions and semantics: the gap is surfaced, not decided;
#     propose_path_heads commits nothing; assign_labor is the only write)
#   - docs/docs/mcp/governance-charter.md (§2 tool curation: intent-level
#     tools; §4 write tools must be annotated and invariant-safe)
#   - apis/openapi.yaml GET /buildings/{buildingId}/shifts/{shiftId}/staffing-gap
#     — the same planned-vs-active read model the gap tool serves.

Feature: MCP tool behavioral evals
  The workforce-management MCP tools expose this bounded context to AI
  agents: planned-vs-active staffing reads, a pure headcount proposal, a
  labor-assignment write, and the curated labor report. An agent relying
  on them must get the same semantics the REST API guarantees, through
  the schema-decoded argument path a model host actually uses.

  Background:
    Given the MCP server is running with the canonical eval state (B1/S1 pack plan of 3 heads; a1 pack, a2 pick, a3 pack+pick, a4 pack on break; one labor-report row)

  Scenario: The staffing gap reads planned vs active heads
    When I call the tool "get_staffing_gap" with arguments
      | buildingId | B1   |
      | shiftId    | S1   |
      | pathId     | pack |
    Then the tool call succeeds
    And the structured result field "buildingId" is "B1"
    And the structured result field "shiftId" is "S1"
    And the structured result field "pathId" is "pack"
    And the structured result field "plannedHeads" is 3
    And the structured result field "activeHeads" is 0
    And the structured result field "understaffed" is true

  Scenario: Reading an understaffed path raises PathUnderstaffed
    A read with a domain-event side effect: the gap tool surfaces the
    shortfall AND publishes PathUnderstaffed so downstream analytics see
    it. Pinned so the side effect cannot be removed silently.
    When I call the tool "get_staffing_gap" with arguments
      | buildingId | B1   |
      | shiftId    | S1   |
      | pathId     | pack |
    Then the tool call succeeds
    And the domain event "PathUnderstaffed" was published

  Scenario: A path absent from the committed plan reports zeros, not an error
    The read model cannot distinguish a path nobody planned from one
    planned for zero heads — both report zeros and are not understaffed.
    Pinned as the visible contract rather than a surprise.
    When I call the tool "get_staffing_gap" with arguments
      | buildingId | B1   |
      | shiftId    | S1   |
      | pathId     | pick |
    Then the tool call succeeds
    And the structured result field "pathId" is "pick"
    And the structured result field "plannedHeads" is 0
    And the structured result field "activeHeads" is 0
    And the structured result field "understaffed" is false

  Scenario: An unknown building or shift is a clean tool error
    When I call the tool "get_staffing_gap" with arguments
      | buildingId | B9   |
      | shiftId    | S9   |
      | pathId     | pack |
    Then the tool call reports a problem mentioning "not found"

  Scenario: A headcount proposal rounds up
    When I call the tool "propose_path_heads" with arguments
      | buildingId | B1   |
      | pathId     | pack |
      | charge     | 100  |
      | plannedRate | 30   |
    Then the tool call succeeds
    And the structured result field "buildingId" is "B1"
    And the structured result field "pathId" is "pack"
    And the structured result field "proposedHeads" is 4
    And the domain event "ShiftPlanProposed" was published

  Scenario: Zero charge needs zero heads
    When I call the tool "propose_path_heads" with arguments
      | buildingId | B1   |
      | pathId     | pack |
      | charge     | 0    |
      | plannedRate | 30   |
    Then the tool call succeeds
    And the structured result field "proposedHeads" is 0

  Scenario: A non-positive planned rate is a clean tool error
    When I call the tool "propose_path_heads" with arguments
      | buildingId | B1   |
      | pathId     | pack |
      | charge     | 100  |
      | plannedRate | 0    |
    Then the tool call reports a problem mentioning "plannedRate"

  Scenario: A negative charge is a clean tool error
    When I call the tool "propose_path_heads" with arguments
      | buildingId | B1   |
      | pathId     | pack |
      | charge     | -1   |
      | plannedRate | 30   |
    Then the tool call reports a problem mentioning "charge"

  Scenario: Assigning a certified associate is visible through the gap
    When I call the tool "assign_labor" with arguments
      | associateId | a1   |
      | pathId      | pack |
    Then the tool call succeeds
    And the structured result field "associateId" is "a1"
    And the structured result field "pathId" is "pack"
    And the structured result field "assignmentId" is "a1@pack"
    And the domain event "LaborAssigned" was published
    When I call the tool "get_staffing_gap" with arguments
      | buildingId | B1   |
      | shiftId    | S1   |
      | pathId     | pack |
    Then the tool call succeeds
    And the structured result field "activeHeads" is 1
    And the structured result field "understaffed" is true

  Scenario: Reassigning an associate moves them, it is not rejected
    The single-active invariant is enforced by construction: a second
    assignment ENDS the prior one and raises LaborReassigned. A model
    calling assign_labor twice gets a move, not a double-booking error.
    When I call the tool "assign_labor" with arguments
      | associateId | a3   |
      | pathId      | pack |
    Then the tool call succeeds
    And the domain event "LaborAssigned" was published
    When I call the tool "assign_labor" with arguments
      | associateId | a3   |
      | pathId      | pick |
    Then the tool call succeeds
    And the structured result field "pathId" is "pick"
    And the structured result field "assignmentId" is "a3@pick"
    And the domain event "LaborReassigned" was published

  Scenario: Assigning an unknown associate is a clean tool error
    When I call the tool "assign_labor" with arguments
      | associateId | ghost |
      | pathId      | pack  |
    Then the tool call reports a problem mentioning "not found"

  Scenario: Assigning an associate without the path certification is rejected
    When I call the tool "assign_labor" with arguments
      | associateId | a2   |
      | pathId      | pack |
    Then the tool call reports a problem mentioning "certification"

  Scenario: Assigning an associate who is on break is rejected
    When I call the tool "assign_labor" with arguments
      | associateId | a4   |
      | pathId      | pack |
    Then the tool call reports a problem mentioning "break"

  Scenario: The labor report serves rows for a window
    When I call the tool "get_workforce_labor_report" with arguments
      | from        | 2026-06-01T00:00:00Z |
      | to          | 2026-06-02T00:00:00Z |
      | pathId      |                      |
      | granularity | hour                 |
    Then the tool call succeeds
    And the structured result field "rows" has 1 entries
    And the structured result row 0 field "pathId" is "pack"
    And the structured result row 0 field "laborAssigned" is 3

  Scenario: A labor report without a window is a clean tool error
    When I call the tool "get_workforce_labor_report" with argument "to" = "2026-06-02T00:00:00Z"
    Then the tool call reports a problem mentioning "from"

  Scenario: Model chatter in the arguments is rejected, not ignored
    The typed tool schemas are strict (additionalProperties: false, the
    SDK default): a host forwarding stray model-generated keys gets a
    clean schema validation error rather than a silent ignore.
    When I call the tool "get_staffing_gap" with arguments
      | buildingId   | B1                          |
      | shiftId      | S1                          |
      | pathId       | pack                        |
      | model_chatter| maybe this path is short?   |
      | step         | 2                           |
    Then the tool call reports a problem mentioning "model_chatter"

  Scenario: A wrong-typed argument is rejected without coercion
    When I call the tool "get_staffing_gap" with argument "pathId" = 42
    Then the tool call does not succeed silently
