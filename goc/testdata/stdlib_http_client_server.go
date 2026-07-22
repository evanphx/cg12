package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
)

func main() {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requestCount.Add(1)
		if request.Method != http.MethodPost {
			http.Error(response, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		if request.URL.Path != "/submit" || request.URL.Query().Get("item") != "gopher" {
			http.Error(response, "wrong target", http.StatusBadRequest)
			return
		}
		if request.Header.Get("X-CG12-Request") != "status" {
			http.Error(response, "wrong header", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil || string(body) != "compiler payload" {
			http.Error(response, "wrong body", http.StatusBadRequest)
			return
		}
		response.Header().Set("X-CG12-Response", "compiled")
		response.WriteHeader(http.StatusCreated)
		_, _ = response.Write([]byte("runtime response"))
	}))

	request, err := http.NewRequest(http.MethodPost, server.URL+"/submit?item=gopher", http.NoBody)
	if err != nil {
		panic(err)
	}
	request.Body = io.NopCloser(&statusReader{data: []byte("compiler payload")})
	request.ContentLength = int64(len("compiler payload"))
	request.Close = true
	request.Header.Set("X-CG12-Request", "status")

	response, err := server.Client().Do(request)
	if err != nil {
		panic(err)
	}
	if response.StatusCode != http.StatusCreated {
		panic("http status mismatch")
	}
	if response.Header.Get("X-CG12-Response") != "compiled" {
		panic("http response header mismatch")
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		panic(err)
	}
	if string(body) != "runtime response" {
		panic("http response body mismatch")
	}
	if requestCount.Load() != 1 {
		panic("http handler request count mismatch")
	}
	if err := response.Body.Close(); err != nil {
		panic(err)
	}
	server.CloseClientConnections()
	server.Close()
}

type statusReader struct {
	data []byte
}

func (reader *statusReader) Read(buffer []byte) (int, error) {
	if len(reader.data) == 0 {
		return 0, io.EOF
	}
	count := copy(buffer, reader.data)
	reader.data = reader.data[count:]
	return count, nil
}
