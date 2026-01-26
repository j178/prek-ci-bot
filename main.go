package main

import (
	"log"
	"net/http"
	"os"

	"github.com/j178/prek-ci-bot/api"
)

func main() {
	http.HandleFunc("/", api.Index)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Listening on :%s...", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
