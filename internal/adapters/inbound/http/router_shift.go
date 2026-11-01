package http

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/claudioed/workforce-management/internal/domain/shared"
)

func (h *Handler) startShift(w http.ResponseWriter, r *http.Request) {
	associateId, err := shared.NewAssociateId(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}

	// Decode into a POINTER so a literal JSON null body (a no-op for a
	// value struct — every field here is optional, so null silently
	// created a shift) is distinguishable and rejected. The spec's
	// requestBody is a required object; null is malformed-request-body.
	var req *startShiftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	if req == nil {
		writeError(w, r, http.StatusBadRequest, errNullBody)
		return
	}

	certs := make([]shared.Certification, len(req.Certifications))
	for i, c := range req.Certifications {
		cert, err := shared.NewCertification(c)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, err)
			return
		}
		certs[i] = cert
	}

	shift, err := h.StartAssociateShift.ExecuteAtSite(r.Context(), associateId, certs, shared.NewSiteCode(req.SiteCode))
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	w.Header().Set("Location", "/associates/"+string(shift.AssociateId()))
	writeJSON(w, http.StatusCreated, map[string]any{
		"associateId": string(shift.AssociateId()),
	})
}

func (h *Handler) certify(w http.ResponseWriter, r *http.Request) {
	associateId, err := shared.NewAssociateId(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}

	var req certifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}

	certification, err := shared.NewCertification(req.Certification)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}

	if err := h.CertifyAssociate.Execute(r.Context(), associateId, certification); err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) assignLabor(w http.ResponseWriter, r *http.Request) {
	associateId, err := shared.NewAssociateId(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}

	var req assignLaborRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	pathId, err := shared.NewPathId(req.PathId)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	if err := h.validatePathId(pathId); err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}

	la, err := h.AssignLabor.Execute(r.Context(), associateId, pathId)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}

	activePathId, active := la.ActivePathId()
	w.Header().Set("Location", "/associates/"+string(associateId)+"/assignments")
	writeJSON(w, http.StatusCreated, assignmentResponse{
		AssociateId:  string(la.AssociateId()),
		ActivePathId: string(activePathId),
		Active:       active,
	})
}

func (h *Handler) startBreak(w http.ResponseWriter, r *http.Request) {
	associateId, err := shared.NewAssociateId(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	if err := h.StartBreak.Execute(r.Context(), associateId); err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) endBreak(w http.ResponseWriter, r *http.Request) {
	associateId, err := shared.NewAssociateId(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	if err := h.EndBreak.Execute(r.Context(), associateId); err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) endShift(w http.ResponseWriter, r *http.Request) {
	associateId, err := shared.NewAssociateId(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	if err := h.EndAssociateShift.Execute(r.Context(), associateId); err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
