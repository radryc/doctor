package query

import (
	"log"
	"net/http"
)

// internalErrorMessage is deliberately generic. Detailed dial/HTTP/storage
// errors are written to the server log with the request ID instead of being
// exposed to callers.
const internalErrorMessage = "internal server error"

func writeInternalError(w http.ResponseWriter, r *http.Request, err error) {
	if r != nil {
		log.Printf("request %s %s failed (requestId=%s): %v", r.Method, r.URL.Path, requestIDFromContext(r), err)
	} else {
		log.Printf("request failed: %v", err)
	}
	response := map[string]string{"error": internalErrorMessage}
	if requestID := requestIDFromContext(r); requestID != "" {
		response["requestId"] = requestID
	}
	writeJSON(w, http.StatusInternalServerError, response)
}

func internalMessage(err error) string {
	if err == nil {
		return internalErrorMessage
	}
	return internalErrorMessage
}
