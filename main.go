package main

import (
	"log"
	"net/http"
	"os"
	"strings"

	"nim-relay/api"
)

func main() {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8080"
	}

	addr := ":" + port
	log.Printf("nim-relay listening on %s (upstream from UPSTREAM_URL)", addr)
	if err := http.ListenAndServe(addr, http.HandlerFunc(api.Handler)); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
