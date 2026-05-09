package main

import (
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/syntaqx/echo-server/handler"
)

func main() {
	port, ok := os.LookupEnv("PORT")
	if !ok {
		port = "8080"
	}

	srv := &http.Server{
		Addr:         net.JoinHostPort("", port),
		Handler:      http.HandlerFunc(handler.EchoHandler),
		IdleTimeout:  time.Minute,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	log.Printf("http server listening at %s\n", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		panic(err)
	}
}
