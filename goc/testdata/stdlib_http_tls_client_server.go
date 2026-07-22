package main

import (
	"io"
	"net/http"
	"net/http/httptest"
)

func main() {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.TLS == nil {
			http.Error(response, "missing TLS state", http.StatusInternalServerError)
			return
		}
		if request.TLS.Version == 0 {
			http.Error(response, "missing TLS version", http.StatusInternalServerError)
			return
		}
		if request.URL.Path != "/secure" {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("X-CG12-TLS", "active")
		_, _ = response.Write([]byte("encrypted response"))
	}))

	response, err := server.Client().Get(server.URL + "/secure")
	if err != nil {
		panic(err)
	}
	if response.TLS == nil || !response.TLS.HandshakeComplete || response.TLS.Version == 0 {
		panic("TLS client handshake state mismatch")
	}
	if response.StatusCode != http.StatusOK {
		panic("TLS HTTP status mismatch")
	}
	if response.Header.Get("X-CG12-TLS") == "" {
		panic("TLS response header mismatch")
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		panic(err)
	}
	if string(body) != "encrypted response" {
		panic("TLS response body mismatch")
	}
	if err := response.Body.Close(); err != nil {
		panic(err)
	}
	server.CloseClientConnections()
	server.Close()
}
