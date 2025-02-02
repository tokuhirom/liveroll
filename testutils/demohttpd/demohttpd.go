package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
)

func main() {
	// Define command-line arguments
	port := flag.String("port", "8080", "Specify the port number")
	content := flag.String("content", "OK", "Specify the response content")
	flag.Parse()

	// Define HTTP handler
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, err := fmt.Fprintln(w, *content)
		if err != nil {
			log.Printf("failed to write response: %v\n", err)
		}
	})

	// Start HTTP server
	addr := fmt.Sprintf(":%s", *port)
	log.Printf("Listening on http://localhost%s\n", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
