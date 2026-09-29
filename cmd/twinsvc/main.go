package main

import (
	"flag"
	"log"
	"net/http"

	"twinsvc/pkg/twin"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	db := flag.String("db", "data/twin.json", "persistence file path")
	flag.Parse()

	svc, err := twin.NewService(twin.NewStore(*db))
	if err != nil {
		log.Fatalf("load state: %v", err)
	}
	srv := twin.NewServer(svc)
	log.Printf("twin service listening on %s, state file %s", *addr, *db)
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}
