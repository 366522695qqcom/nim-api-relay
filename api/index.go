package api

import (
	"net/http"

	"nim-relay/internal/relay"
)

// Handler is the Vercel serverless entry point. Vercel routes incoming
// HTTP requests to this exported function.
func Handler(w http.ResponseWriter, r *http.Request) {
	relay.NewHandler().ServeHTTP(w, r)
}
