package query

import (
	"log"
	"net/http"
	"strings"
)

// internalErrorMessage is deliberately generic. Detailed dial/HTTP/storage
// errors are written to the server log with the request ID instead of being
// exposed to callers.
const internalErrorMessage = "internal server error"

// Sanitised cause values returned alongside internalErrorMessage. They let
// machine callers (the doctor UI, Nightwatch) attribute a failure to the right
// subsystem without leaking dial strings or storage internals.
const (
	causeInternal           = "internal"
	causeStorageUnavailable = "storage_unavailable"
	causeBackendUnavailable = "backend_unavailable"
	causeBackendTimeout     = "backend_timeout"
	causeAccessDenied       = "access_denied"
)

// internalCause classifies an error message into a small, stable vocabulary.
// Keep this conservative: only claim a specific cause when the error text is
// unambiguous.
func internalCause(message string) string {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "no healthy nodes"):
		return causeStorageUnavailable
	case strings.Contains(lower, "permission denied"),
		strings.Contains(lower, "access denied"),
		strings.Contains(lower, "unauthorized"):
		return causeAccessDenied
	case strings.Contains(lower, "context deadline exceeded"),
		strings.Contains(lower, "deadline exceeded"),
		strings.Contains(lower, "timeout"),
		strings.Contains(lower, "timed out"):
		return causeBackendTimeout
	case strings.Contains(lower, "connection refused"),
		strings.Contains(lower, "connection reset"),
		strings.Contains(lower, "broken pipe"),
		strings.Contains(lower, "no such host"),
		strings.Contains(lower, "unavailable"):
		return causeBackendUnavailable
	default:
		return causeInternal
	}
}

func writeInternalError(w http.ResponseWriter, r *http.Request, err error) {
	if r != nil {
		log.Printf("request %s %s failed (requestId=%s): %v", r.Method, r.URL.Path, requestIDFromContext(r), err)
	} else {
		log.Printf("request failed: %v", err)
	}
	response := map[string]string{
		"error": internalErrorMessage,
		"cause": causeInternal,
	}
	if err != nil {
		response["cause"] = internalCause(err.Error())
	}
	if r != nil {
		if requestID := requestIDFromContext(r); requestID != "" {
			response["requestId"] = requestID
		}
	}
	writeJSON(w, http.StatusInternalServerError, response)
}

func internalMessage(err error) string {
	if err == nil {
		return internalErrorMessage
	}
	return internalErrorMessage
}
