package main

import (
	"fmt"
	"log"
	"net/http"
)

func healthHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "OK")
}

func main() {
	http.HandleFunc("/health", healthHandler)

	log.Println("Auth service running on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

