# Derived from:
# - apis/openapi.yaml — POST /paths/{pathId}/plan/propose: "Computes
#   heads = ceil(charge / plannedRate) for one path ... it persists
#   nothing and commits nothing ... this is why the response is 200, not
#   201: no resource is created"; ProposePathPlanResponse documents
#   proposedHeads/resolvedRate/rateSource ("caller").
# - .claude/rules/domain-model.md — use case 3 ProposePathPlan: "pure
#   computation: heads = ceil(charge / resolvedRate); does not commit",
#   response includes resolvedRate + rateSource.
Feature: Proposing a path plan is pure computation
  As a shift manager
  I want the software to propose heads for a path from a charge and a rate
  So that a human can commit an informed ShiftPlan without the proposal committing anything

  @bdd
  Scenario: Proposing heads from a caller-supplied rate computes the ceiling without committing
    When a path plan is proposed for path "pack" building "bldg-1" with charge 100 and planned rate 30
    Then the proposal suggests 4 heads at resolved rate 30 from source "caller"
