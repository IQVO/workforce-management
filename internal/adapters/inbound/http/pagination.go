package http

import (
	"net/http"
	"sort"

	"github.com/claudioed/workforce-management/internal/adapters/inbound/http/page"
	"github.com/claudioed/workforce-management/internal/application/usecases"
)

// pageParams validates the paging query parameters of a collection GET
// (fleet REST guideline 3/4.2): an undeclared query parameter is 400
// unknown-query-parameter, a bad limit 400 invalid-limit, a bad cursor 400
// invalid-cursor. It writes the problem and returns ok=false on failure.
func pageParams(w http.ResponseWriter, r *http.Request, filters ...string) (limit int, after *page.Position, ok bool) {
	if name := page.Unknown(r, filters...); name != "" {
		writeProblem(w, http.StatusBadRequest,
			problemCategory{"unknown-query-parameter", "Unknown query parameter"},
			"query parameter "+name+" is not declared by this collection", r.URL.Path)
		return 0, nil, false
	}
	limit, err := page.Limit(r)
	if err != nil {
		writeProblem(w, http.StatusBadRequest,
			problemCategory{"invalid-limit", "Invalid limit"}, err.Error(), r.URL.Path)
		return 0, nil, false
	}
	after, err = page.After(r)
	if err != nil {
		writeProblem(w, http.StatusBadRequest,
			problemCategory{"invalid-cursor", "Invalid cursor"}, err.Error(), r.URL.Path)
		return 0, nil, false
	}
	return limit, after, true
}

// staffingGapPage keyset-pages the per-path gaps of one committed plan. The
// gaps are derived from the plan's lines (a bounded in-memory set, no SQL
// query to page), ordered by pathId, which is unique within a plan, so the
// cursor {s: pathId, i: pathId} never skips or repeats a row.
func staffingGapPage(gaps []usecases.StaffingGap, limit int, after *page.Position) page.Page[staffingGapResponse] {
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].PathId < gaps[j].PathId })
	rows := make([]usecases.StaffingGap, 0, page.FetchSize(limit))
	for _, g := range gaps {
		if after != nil && string(g.PathId) <= after.ID {
			continue
		}
		rows = append(rows, g)
		if len(rows) == page.FetchSize(limit) {
			break
		}
	}
	return pageOfGaps(rows, limit)
}

func pageOfGaps(rows []usecases.StaffingGap, limit int) page.Page[staffingGapResponse] {
	p := page.Of(rows, limit, func(g usecases.StaffingGap) page.Position {
		return page.Position{Sort: string(g.PathId), ID: string(g.PathId)}
	})
	return page.Page[staffingGapResponse]{Items: toStaffingGapResponses(p.Items), NextCursor: p.NextCursor}
}
