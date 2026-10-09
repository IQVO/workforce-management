# Liveness and readiness.
#
# Derived from: apis/openapi.yaml /healthz and /readyz (ADR-0022 graceful
# shutdown): liveness never flips; readiness answers 200 {"status":"ready"}
# while serving and 503 {"status":"not_ready"} once shutdown begins, so a
# Kubernetes readinessProbe stops routing traffic during the drain window.

Feature: Reporting health
  As the platform
  I want a draining pod to leave rotation before it stops serving
  So that a deploy never drops in-flight requests

  @bdd
  Scenario: A serving pod is live and ready
    When a GET request is sent to "/healthz"
    Then the last request succeeds with status 200
    When a GET request is sent to "/readyz"
    Then the last request succeeds with status 200
    And the response field "status" is "ready"

  @bdd
  Scenario: A draining pod stops being ready but stays live
    When the service begins graceful shutdown
    And a GET request is sent to "/readyz"
    Then the request is rejected with status 503
    And the response field "status" is "not_ready"
    When a GET request is sent to "/healthz"
    Then the last request succeeds with status 200
