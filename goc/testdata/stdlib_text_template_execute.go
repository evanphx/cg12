package main

import (
	"bytes"
	"text/template"
)

type templateItem struct {
	Name  string
	Value int
}

func main() {
	source := `{{range .}}{{.Name}}={{.Value}};{{end}}`
	parsed, err := template.New("items").Parse(source)
	if err != nil {
		panic("template Parse failed")
	}

	items := []templateItem{
		{Name: "alpha", Value: 17},
		{Name: "beta", Value: 25},
	}
	var output bytes.Buffer
	if err := parsed.Execute(&output, items); err != nil {
		panic("template Execute failed")
	}
	if output.String() != "alpha=17;beta=25;" {
		panic("template produced wrong output")
	}
}
