package main

import (
	"os"
	"text/template"
)

func main() {
	source := `{{range .}}{{.Name}}={{.Value}};{{end}}`
	parsed, err := template.New("items").Parse(source)
	if err != nil {
		os.Stdout.WriteString(err.Error())
		return
	}
	if parsed == nil {
		os.Stdout.WriteString("nil template")
		return
	}
	os.Stdout.WriteString("ok")
}
