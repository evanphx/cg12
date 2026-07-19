package main

import "reflect"

func main() {
	mapType := reflect.TypeOf(map[string]int{})
	value := reflect.MakeMap(mapType)
	value.SetMapIndex(reflect.ValueOf("a"), reflect.ValueOf(17))
	value.SetMapIndex(reflect.ValueOf("b"), reflect.ValueOf(25))

	total := 0
	for _, key := range value.MapKeys() {
		total += int(value.MapIndex(key).Int())
	}
	if total != 42 {
		panic("reflect map result mismatch")
	}

	sliceType := reflect.TypeOf([]int{})
	slice := reflect.MakeSlice(sliceType, 0, 2)
	slice = reflect.Append(slice, reflect.ValueOf(19))
	slice = reflect.Append(slice, reflect.ValueOf(23))
	if slice.Len() != 2 || int(slice.Index(0).Int()+slice.Index(1).Int()) != 42 {
		panic("reflect slice result mismatch")
	}
}
