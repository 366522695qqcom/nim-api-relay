package main

import (
	"log"
	"net/http"
	"os"
	"strings"

	"nim-relay/internal/relay"
)

func main() {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8080"
	}

	handler := relay.NewHandler()

	addr := ":" + port
	log.Printf("nim-relay listening on %s (upstream from UPSTREAM_URL)", addr)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
