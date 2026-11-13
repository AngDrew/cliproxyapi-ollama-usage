// HTTPS Ollama fixture for integration.py; reachable only on an internal test network.
package main

import (
	"fmt"
	"net/http"
)

func main() {
	http.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		key := r.Header.Get("Authorization")
		if key == "Bearer rate-limited" {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":"PRIVATE-DO-NOT-ECHO"}`)
			return
		}
		if key != "Bearer fixture-key" && key != "Bearer legacy-key" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":"PRIVATE-DO-NOT-ECHO"}`)
			return
		}
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		switch r.URL.RequestURI() {
		case "/api/balance":
			if key == "Bearer legacy-key" {
				fmt.Fprint(w, `{"included":{"session":{"remaining_percent":75,"resets_at":"2099-10-01T07:00:00Z"},"weekly":{"remaining_percent":40,"resets_at":"2099-10-05T00:00:00Z"}},"purchased":{"balance_usd":25}}`)
			} else {
				fmt.Fprint(w, `{"included":{"balance_usd":72.5,"allowance_usd":100,"period":{"from":"2099-09-15T09:30:00Z","until":"2099-10-15T09:30:00Z"}},"purchased":{"balance_usd":25}}`)
			}
		case "/api/usage?range=24h":
			fmt.Fprint(w, `{"range":"24h","totals":{"usage_usd":0.01718,"request_count":15,"input_tokens":106000,"cached_input_tokens":46000,"output_tokens":13600}}`)
		default:
			w.WriteHeader(404)
		}
	})
	fmt.Println("fixture ready")
	if err := http.ListenAndServeTLS(":443", "/test/ca.crt", "/test/ca.key", nil); err != nil {
		panic(err)
	}
}
