package main

import "text/template"

func main() {
	source := `{{define "item"}}{{.Name}}={{.Value}}{{end}}{{range .}}{{template "item" .}};{{end}}`
	parsed, err := template.New("items").Parse(source)
	if err != nil {
		panic("template Parse failed")
	}
	if parsed.Lookup("item") == nil {
		panic("template Lookup failed")
	}
}
