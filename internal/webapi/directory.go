package webapi

import (
	"aiharn/internal/sessions"
	"net/http"
)

func workingDirectory(h sessions.Handle) string {
	if d, ok := h.(sessions.WorkingDirectoryHandle); ok {
		return d.WorkingDirectory()
	}
	return ""
}
func (s *Server) handleSessionDirectory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
		Directory string `json:"directory"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}
	h, ok := s.resolveOrNotFound(w, body.SessionID)
	if !ok {
		return
	}
	d, ok := h.(sessions.WorkingDirectoryHandle)
	if !ok {
		writeError(w, http.StatusNotImplemented, "directory control unavailable")
		return
	}
	if err := d.SetWorkingDirectory(r.Context(), body.Directory); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"working_directory": d.WorkingDirectory()})
}
