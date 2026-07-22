package main

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
)

func main() {
	var encoded bytes.Buffer
	writer := multipart.NewWriter(&encoded)
	if err := writer.WriteField("project", "cg12"); err != nil {
		panic(err)
	}
	if err := writer.WriteField("stage", "runtime-status"); err != nil {
		panic(err)
	}
	file, err := writer.CreateFormFile("source", "main.go")
	if err != nil {
		panic(err)
	}
	if _, err := file.Write([]byte("package main\nfunc main() {}\n")); err != nil {
		panic(err)
	}
	if err := writer.Close(); err != nil {
		panic(err)
	}

	request, err := http.NewRequest(http.MethodPost, "http://compiler.example/build", bytes.NewReader(encoded.Bytes()))
	if err != nil {
		panic(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	if err := request.ParseMultipartForm(1 << 20); err != nil {
		panic(err)
	}
	defer request.MultipartForm.RemoveAll()
	if request.FormValue("project") != "cg12" || request.FormValue("stage") != "runtime-status" {
		panic("multipart form field mismatch")
	}

	upload, header, err := request.FormFile("source")
	if err != nil {
		panic(err)
	}
	defer upload.Close()
	if header.Filename != "main.go" {
		panic("multipart filename mismatch")
	}
	contents, err := io.ReadAll(upload)
	if err != nil {
		panic(err)
	}
	if string(contents) != "package main\nfunc main() {}\n" {
		panic("multipart file contents mismatch")
	}
}
