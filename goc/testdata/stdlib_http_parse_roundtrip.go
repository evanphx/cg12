package main

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strings"
)

func main() {
	rawRequest := "POST /upload?mode=status HTTP/1.1\r\n" +
		"Host: compiler.example\r\n" +
		"X-CG12: first\r\n" +
		"X-CG12: second\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Trailer: X-Checksum\r\n" +
		"\r\n" +
		"8\r\ncompiler\r\n" +
		"7\r\n status\r\n" +
		"0\r\n" +
		"X-Checksum: verified\r\n" +
		"\r\n"
	request, err := http.ReadRequest(bufio.NewReader(strings.NewReader(rawRequest)))
	if err != nil {
		panic(err)
	}
	if request.Method != http.MethodPost || request.Host != "compiler.example" {
		panic("parsed request line mismatch")
	}
	if request.URL.Path != "/upload" || request.URL.Query().Get("mode") != "status" {
		panic("parsed request URL mismatch")
	}
	if len(request.Header.Values("X-CG12")) != 2 {
		panic("parsed repeated header mismatch")
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		panic(err)
	}
	if string(body) != "compiler status" {
		panic("parsed chunked body mismatch")
	}
	if request.Trailer.Get("X-Checksum") != "verified" {
		panic("parsed trailer mismatch")
	}

	response := &http.Response{
		StatusCode:    http.StatusAccepted,
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader("round trip response")),
		ContentLength: int64(len("round trip response")),
		Request:       request,
	}
	response.Header.Set("Content-Type", "text/plain")
	var encoded bytes.Buffer
	if err := response.Write(&encoded); err != nil {
		panic(err)
	}
	parsedResponse, err := http.ReadResponse(bufio.NewReader(&encoded), request)
	if err != nil {
		panic(err)
	}
	if parsedResponse.StatusCode != http.StatusAccepted {
		panic("response status round trip mismatch")
	}
	responseBody, err := io.ReadAll(parsedResponse.Body)
	if err != nil {
		panic(err)
	}
	if string(responseBody) != "round trip response" {
		panic("response body round trip mismatch")
	}
}
