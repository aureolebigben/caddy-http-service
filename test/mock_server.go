// Mock server for testing http_service module
// Run: go run mock_server.go
//
// Then run Caddy: xcaddy run -- --config test/Caddyfile.test
package main

import (
	"encoding/json"
	"log"
	"net/http"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		result := map[string]string{
			"domain": q.Get("domain"),
			"name":   q.Get("name"),
			"page":   q.Get("page"),
			"q":      q.Get("q"),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	})

	log.Println("Mock server listening on :8888")
	log.Fatal(http.ListenAndServe(":8888", nil))
}
