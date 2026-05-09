package handler

import (
	"io"
	"net/http"
)

const HelloWorld = "Hello, world!"

func EchoHandler(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, HelloWorld)
}
