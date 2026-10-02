package main

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"nim-relay/api"
)

func main() {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8080"
	}

	addr := ":" + port
	srv := &http.Server{
		Addr:              addr,
		Handler:           http.HandlerFunc(api.Handler),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10, // 8 KiB
	}
	log.Printf("nim-relay listening on %s (upstream from UPSTREAM_URL)", addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}