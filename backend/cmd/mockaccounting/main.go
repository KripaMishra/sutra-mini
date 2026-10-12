// Command mockaccounting serves the deliberately unreliable accounting API —
// 20% of calls return HTTP 500, 10% hang for five seconds and then succeed,
// and a repeated client_ref returns 409 with the reference it already holds.
// The contract and its tests live in internal/mockacct.
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/kripa5661/srs/backend/internal/mockacct"
)

func main() {
	addr := os.Getenv("MOCK_ADDR")
	if addr == "" {
		addr = ":8090"
	}
	log.Printf("mock accounting listening on %s", addr)
	if err := http.ListenAndServe(addr, mockacct.NewHandler(mockacct.Options{})); err != nil {
		log.Fatal(err)
	}
}
