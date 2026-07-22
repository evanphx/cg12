package main

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"time"
)

func main() {
	jar, err := cookiejar.New(nil)
	if err != nil {
		panic(err)
	}
	httpsURL := mustParseURL("https://api.example.com/account/profile")
	jar.SetCookies(httpsURL, []*http.Cookie{
		{Name: "host", Value: "api", Path: "/"},
		{Name: "account", Value: "private", Path: "/account"},
		{Name: "secure", Value: "yes", Path: "/", Secure: true},
		{Name: "expired", Value: "gone", Path: "/", Expires: time.Unix(1, 0)},
	})

	accountCookies := jar.Cookies(httpsURL)
	if cookieValue(accountCookies, "host") != "api" {
		panic("cookie jar host cookie mismatch")
	}
	if cookieValue(accountCookies, "account") != "private" {
		panic("cookie jar path cookie mismatch")
	}
	if cookieValue(accountCookies, "secure") != "yes" {
		panic("cookie jar secure cookie mismatch")
	}
	if cookieValue(accountCookies, "expired") != "" {
		panic("cookie jar retained expired cookie")
	}

	otherPath := mustParseURL("https://api.example.com/public")
	if cookieValue(jar.Cookies(otherPath), "account") != "" {
		panic("cookie jar leaked path cookie")
	}
	insecureURL := mustParseURL("http://api.example.com/account/profile")
	if cookieValue(jar.Cookies(insecureURL), "secure") != "" {
		panic("cookie jar leaked secure cookie")
	}
	otherHost := mustParseURL("https://www.example.com/account/profile")
	if len(jar.Cookies(otherHost)) != 0 {
		panic("cookie jar leaked host-only cookies")
	}
}

func mustParseURL(value string) *url.URL {
	parsed, err := url.Parse(value)
	if err != nil {
		panic(err)
	}
	return parsed
}

func cookieValue(cookies []*http.Cookie, name string) string {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie.Value
		}
	}
	return ""
}
