// Command orb-registry-catalog serves the HTTP API described in README.md.
package main

import (
	"log"
	"os"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/api"
	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
)

func main() {
	address := os.Getenv("ADDR")
	if address == "" {
		address = "127.0.0.1:8080"
	}
	databasePath := os.Getenv("DB_PATH")
	if databasePath == "" {
		databasePath = "orb-registry-catalog.db"
	}

	st, err := store.Open(databasePath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// The SQLite store owns its own lifecycle; the router only receives the
	// storage port plus the independent health probe, and never closes either.
	if err := api.NewRouterWithHealth(st, st.Ping).Run(address); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
