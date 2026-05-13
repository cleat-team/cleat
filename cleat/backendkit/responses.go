package backendkit

import (
	"encoding/json"
	"net/http"
)

// WriteJSON marshals data as JSON and writes it to the response with the given status code.
func WriteJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// WriteError writes a JSON error response with the given status code and message.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}

// WriteValidationError is a shortcut for 400 Bad Request.
func WriteValidationError(w http.ResponseWriter, msg string) {
	WriteError(w, http.StatusBadRequest, msg)
}

// WriteNotFound is a shortcut for 404 Not Found.
func WriteNotFound(w http.ResponseWriter) {
	WriteError(w, http.StatusNotFound, "not found")
}

// WriteInternalError is a shortcut for 500 Internal Server Error.
func WriteInternalError(w http.ResponseWriter) {
	WriteError(w, http.StatusInternalServerError, "internal server error")
}
