package main

import (
	"fmt"
	"os"
	"reflect"
	"strings"

	"modernc.org/cc/v4"
)

func dump(v reflect.Value, depth int, w *strings.Builder) {
	if depth > 40 {
		return
	}
	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return
	}
	t := v.Type()
	if t.Name() == "Token" {
		return
	}
	line := t.Name()
	if c := v.FieldByName("Case"); c.IsValid() {
		line += " [" + fmt.Sprint(c.Interface()) + "]"
	}
	// capture an identifier token if present
	if tok := v.FieldByName("Token"); tok.IsValid() {
		if m := tok.MethodByName("SrcStr"); m.IsValid() {
			s := m.Call(nil)[0].String()
			if s != "" {
				line += " '" + s + "'"
			}
		}
	}
	fmt.Fprintf(w, "%s%s\n", strings.Repeat("  ", depth), line)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" || f.Name == "Case" || f.Name == "Token" {
			continue
		}
		dump(v.Field(i), depth+1, w)
	}
}

func main() {
	cfg, _ := cc.NewConfig("linux", "arm64")
	ast, err := cc.Translate(cfg, []cc.Source{
		{Name: "<predefined>", Value: cfg.Predefined},
		{Name: "<builtin>", Value: cc.Builtin},
		{Name: "main.c", Value: os.Args[1]},
	})
	if err != nil {
		fmt.Println("translate:", err)
		return
	}
	// Only dump external declarations from main.c
	var w strings.Builder
	for tu := ast.TranslationUnit; tu != nil; tu = tu.TranslationUnit {
		ed := tu.ExternalDeclaration
		if ed == nil || !strings.Contains(ed.Position().Filename, "main.c") {
			continue
		}
		dump(reflect.ValueOf(ed), 0, &w)
	}
	fmt.Print(w.String())
}
