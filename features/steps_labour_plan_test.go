package bdd

// ADR 0043 step definitions: warehouse-planning's published labour plan,
// seeded into the local copy exactly as the labour-plan consumer would store
// it. Every assertion still goes through the REST boundary.

import (
	"context"

	"github.com/cucumber/godog"

	"github.com/claudioed/workforce-management/internal/domain/labourplan"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// labourPlanPublished stores one LabourPlanCommitted row (version 1) in the
// scenario's copy.
func (w *world) labourPlanPublished(heads int, rate, site, shift, path string) error {
	r, err := parseFloat(rate)
	if err != nil {
		return err
	}
	return w.h.requirements.Save(context.Background(), labourplan.Requirement{
		Key:              labourplan.Key{SiteCode: site, ShiftId: shift, PathId: shared.PathId(path)},
		PlannedHeads:     heads,
		RateUnitsPerHour: r,
		Hours:            8,
		HeadsSource:      labourplan.HeadsSourceForecast,
		Version:          1,
	})
}

func registerLabourPlan(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^warehouse-planning has published a labour plan of (\d+) heads? at rate ([0-9.]+) for site "([^"]*)" shift "([^"]*)" path "([^"]*)"$`, w.labourPlanPublished)
}
