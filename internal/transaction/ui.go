package transaction

import (
	_ "embed"
	"net/http"
)

//go:embed ui.html
var uiHTML []byte

// HandleServeUI renders the embedded Bank Connections Web UI.
func (h *Handler) HandleServeUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(uiHTML)
}
