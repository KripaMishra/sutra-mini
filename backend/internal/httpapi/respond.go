package httpapi

import (
	"encoding/json"
	"log"
	"net/http"
)

// Transport helpers shared by every handler: how this package shapes a
// response, and how it complains when something internal breaks.

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// logInternal keeps the detail server-side; the client gets a generic message.
// It is always paired with writeError(w, http.StatusInternalServerError, ...).
func logInternal(op string, err error) {
	log.Printf("error: %s: %v", op, err)
}
