package main

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"sync"
)

func main() {
	var mutex sync.Mutex
	var remoteAddresses []string
	requestCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		requestCount++
		remoteAddresses = append(remoteAddresses, request.RemoteAddr)
		mutex.Unlock()

		switch request.URL.Path {
		case "/start":
			http.SetCookie(response, &http.Cookie{
				Name:  "cg12-session",
				Value: "redirected",
				Path:  "/",
			})
			http.Redirect(response, request, "/final?step=2", http.StatusFound)
		case "/final":
			cookie, err := request.Cookie("cg12-session")
			if err != nil || cookie.Value != "redirected" {
				http.Error(response, "redirect cookie missing", http.StatusBadRequest)
				return
			}
			if request.URL.Query().Get("step") != "2" {
				http.Error(response, "redirect query missing", http.StatusBadRequest)
				return
			}
			if request.Header.Get("X-CG12-Redirect") != "preserved" {
				http.Error(response, "redirect header missing", http.StatusBadRequest)
				return
			}
			response.Header().Set("X-CG12-Final", "complete")
			_, _ = response.Write([]byte("redirect complete"))
		default:
			http.NotFound(response, request)
		}
	}))

	jar, err := cookiejar.New(nil)
	if err != nil {
		panic(err)
	}
	client := server.Client()
	client.Jar = jar

	request, err := http.NewRequest(http.MethodGet, server.URL+"/start", nil)
	if err != nil {
		panic(err)
	}
	request.Header.Set("X-CG12-Redirect", "preserved")

	response, err := client.Do(request)
	if err != nil {
		panic(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		panic(err)
	}
	if err := response.Body.Close(); err != nil {
		panic(err)
	}
	if response.StatusCode != http.StatusOK {
		panic("redirect status mismatch")
	}
	if response.Request.URL.Path != "/final" {
		panic("redirect target mismatch")
	}
	if response.Header.Get("X-CG12-Final") != "complete" {
		panic("redirect response header mismatch")
	}
	if string(body) != "redirect complete" {
		panic("redirect response body mismatch")
	}

	mutex.Lock()
	if requestCount != 2 {
		mutex.Unlock()
		panic("redirect request count mismatch")
	}
	if len(remoteAddresses) != 2 || remoteAddresses[0] != remoteAddresses[1] {
		mutex.Unlock()
		panic("redirect did not reuse the HTTP connection")
	}
	mutex.Unlock()

	server.CloseClientConnections()
	server.Close()
}
