package main

import "reflect"

type metadataPayload struct {
	Name   string
	Values []int `goc:"values"`
}

func main() {
	valueType := reflect.TypeOf(metadataPayload{})
	if valueType.Name() != "metadataPayload" {
		panic("wrong reflect type name")
	}
	if valueType.Kind() != reflect.Struct {
		panic("wrong reflect kind")
	}
	if valueType.NumField() != 2 {
		panic("wrong reflect field count")
	}

	field := valueType.Field(1)
	if field.Name != "Values" || field.Tag.Get("goc") != "values" {
		panic("wrong reflect field metadata")
	}
	if field.Type.Kind() != reflect.Slice || field.Type.Elem().Kind() != reflect.Int {
		panic("wrong reflect field type metadata")
	}
}
