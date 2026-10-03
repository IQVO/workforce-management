// Package architecture contains fitness tests (the Go equivalent of Java's
// ArchUnit, via github.com/arch-go/arch-go) that enforce this codebase's
// hexagonal / ports-and-adapters dependency rule as executable tests rather
// than as documentation someone has to remember to honor.
package architecture

import (
	"strings"
	"testing"

	archgo "github.com/arch-go/arch-go/api"
	config "github.com/arch-go/arch-go/api/configuration"
)

const modulePath = "github.com/claudioed/workforce-management"

// reportFailure turns a failed dependencies rule result into a readable
// t.Errorf listing exactly which package(s) violated it and how, so a CI
// failure points straight at the offending import instead of just saying
// "architecture test failed".
func reportFailure(t *testing.T, result *archgo.Result) {
	t.Helper()

	if result.DependenciesRuleResult == nil {
		t.Fatal("expected a dependencies rule result, got none")
	}

	for _, rule := range result.DependenciesRuleResult.Results {
		if rule.Passes {
			continue
		}

		for _, v := range rule.Verifications {
			if v.Passes {
				continue
			}

			t.Errorf("%s", archViolation("dependency", "package "+v.Package+" violates "+rule.Description, strings.Join(v.Details, "; ")))
		}
	}
}

// runDependenciesRule loads this module's real package graph and checks a
// single dependencies rule against it, failing the (sub)test with the
// offending package/import when the rule is violated.
func runDependenciesRule(t *testing.T, rule *config.DependenciesRule) {
	t.Helper()

	moduleInfo := config.Load(modulePath)

	result := archgo.CheckArchitecture(moduleInfo, config.Config{
		DependenciesRules: []*config.DependenciesRule{rule},
	})

	if !result.Pass {
		reportFailure(t, result)
	}
}

// TestHexagonalDependencyRule encodes, as separate subtests, the dependency
// rule described in CLAUDE.md and ARCH_TEST_TASK.md: dependencies point
// inward only, and adapters never depend on each other directly.
func TestHexagonalDependencyRule(t *testing.T) {
	t.Run("domain depends on nothing internal except domain", func(t *testing.T) {
		runDependenciesRule(t, &config.DependenciesRule{
			Package: "**.internal.domain.**",
			ShouldOnlyDependsOn: &config.Dependencies{
				Internal: []string{"**.internal.domain.**"},
			},
		})
	})

	t.Run("application depends only on domain", func(t *testing.T) {
		runDependenciesRule(t, &config.DependenciesRule{
			Package: "**.internal.application.**",
			ShouldOnlyDependsOn: &config.Dependencies{
				Internal: []string{
					"**.internal.domain.**",
					"**.internal.application.**",
				},
			},
		})
	})

	t.Run("inbound adapters do not depend on outbound adapters", func(t *testing.T) {
		runDependenciesRule(t, &config.DependenciesRule{
			Package: "**.internal.adapters.inbound.**",
			ShouldNotDependsOn: &config.Dependencies{
				Internal: []string{"**.internal.adapters.outbound.**"},
			},
		})
	})

	t.Run("outbound adapters do not depend on inbound adapters", func(t *testing.T) {
		runDependenciesRule(t, &config.DependenciesRule{
			Package: "**.internal.adapters.outbound.**",
			ShouldNotDependsOn: &config.Dependencies{
				Internal: []string{"**.internal.adapters.inbound.**"},
			},
		})
	})

	t.Run("only cmd wires everything, nothing else depends on cmd", func(t *testing.T) {
		runDependenciesRule(t, &config.DependenciesRule{
			Package: "**.internal.**",
			ShouldNotDependsOn: &config.Dependencies{
				Internal: []string{"**.cmd.**"},
			},
		})
	})

	// ADR-0010: the analytics data product is additive and isolated. The OLTP
	// domain and application layers must never import the analytical read model
	// region or its store adapter, so analytics work can never couple back into
	// the transactional core.
	t.Run("OLTP domain does not import the analytics region or store", func(t *testing.T) {
		runDependenciesRule(t, &config.DependenciesRule{
			Package: "**.internal.domain.**",
			ShouldNotDependsOn: &config.Dependencies{
				Internal: []string{
					"**.internal.analytics.**",
					"**.internal.adapters.outbound.analyticsstore.**",
				},
			},
		})
	})

	t.Run("OLTP application does not import the analytics region or store", func(t *testing.T) {
		runDependenciesRule(t, &config.DependenciesRule{
			Package: "**.internal.application.**",
			ShouldNotDependsOn: &config.Dependencies{
				Internal: []string{
					"**.internal.analytics.**",
					"**.internal.adapters.outbound.analyticsstore.**",
				},
			},
		})
	})

	// The analytics read-model region depends on nothing else in this module —
	// not the OLTP domain, not the application layer, not any adapter.
	t.Run("analytics report region depends on nothing internal", func(t *testing.T) {
		runDependenciesRule(t, &config.DependenciesRule{
			Package: "**.internal.analytics.**",
			ShouldNotDependsOn: &config.Dependencies{
				Internal: []string{
					"**.internal.domain.**",
					"**.internal.application.**",
					"**.internal.adapters.**",
				},
			},
		})
	})
}

// TestPortsPackageIsInterfacesOnly is a bonus content-convention check: this
// codebase's application/ports package exists solely to declare the OUT
// ports (AssociateRepo, ShiftPlanRepo, AssignmentRepo, EventPublisher,
// Clock) that adapters implement — it must never accumulate structs or
// helper functions, or it stops being a clean port boundary.
func TestPortsPackageIsInterfacesOnly(t *testing.T) {
	moduleInfo := config.Load(modulePath)

	result := archgo.CheckArchitecture(moduleInfo, config.Config{
		ContentRules: []*config.ContentsRule{
			{
				Package:                     "**.internal.application.ports",
				ShouldOnlyContainInterfaces: true,
			},
		},
	})

	if !result.Pass {
		if result.ContentsRuleResult == nil {
			t.Fatal("expected a contents rule result, got none")
		}

		for _, rule := range result.ContentsRuleResult.Results {
			if rule.Passes {
				continue
			}

			for _, v := range rule.Verifications {
				if v.Passes {
					continue
				}

				t.Errorf("%s", archViolation("dependency", "package "+v.Package+" violates "+rule.Description, strings.Join(v.Details, "; ")))
			}
		}
	}
}
