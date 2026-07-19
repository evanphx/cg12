package main

import "reflect"

type reflectSetPayload struct {
	Name  string
	Count int
}

func main() {
	payload := &reflectSetPayload{}
	value := reflect.ValueOf(payload).Elem()
	value.FieldByName("Name").SetString("updated")
	value.FieldByName("Count").SetInt(37)

	if payload.Name != "updated" || payload.Count != 37 {
		panic("reflect field set did not update struct")
	}
}
