package webapi

import (
	"net/http"

	"aiharn/internal/sessions"
)

func workingDirectory(h sessions.Handle) string {
	if d, ok := h.(sessions.WorkingDirectoryHandle); ok {
		return d.WorkingDirectory()
	}
	return ""
}
func (s *Server) handleSessionDirectory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		methodNotAllowed(w, "GET, POST")
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
		Directory string `json:"directory"`
		Channel   string `json:"channel"`
	}
	if r.Method == http.MethodGet {
		body.SessionID = r.URL.Query().Get("session_id")
		body.Channel = r.URL.Query().Get("channel")
	} else if err := decodeJSON(w, r, &body); err != nil {
		return
	}
	h, ok := s.resolveOrNotFound(w, body.SessionID)
	if !ok {
		return
	}
	if d, ok := h.(sessions.ChannelDirectoryHandle); ok {
		var dir *string
		if r.Method == http.MethodPost {
			dir = &body.Directory
		}
		value, err := d.ChannelWorkingDirectory(r.Context(), body.Channel, dir)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"working_directory": value})
		return
	}
	d, ok := h.(sessions.WorkingDirectoryHandle)
	if !ok {
		writeError(w, http.StatusNotImplemented, "directory control unavailable")
		return
	}
	if r.Method == http.MethodPost {
		if err := d.SetWorkingDirectory(r.Context(), body.Directory); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"working_directory": d.WorkingDirectory()})
}
