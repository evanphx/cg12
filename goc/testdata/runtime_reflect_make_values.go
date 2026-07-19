package main

import "reflect"

func main() {
	sliceType := reflect.TypeOf([]int{})
	slice := reflect.MakeSlice(sliceType, 0, 4)
	slice = reflect.Append(slice, reflect.ValueOf(17))
	slice = reflect.Append(slice, reflect.ValueOf(25))

	mapType := reflect.TypeOf(map[string]int{})
	mapping := reflect.MakeMap(mapType)
	mapping.SetMapIndex(reflect.ValueOf("left"), slice.Index(0))
	mapping.SetMapIndex(reflect.ValueOf("right"), slice.Index(1))

	total := mapping.MapIndex(reflect.ValueOf("left")).Int()
	total += mapping.MapIndex(reflect.ValueOf("right")).Int()
	if total != 42 {
		panic("reflect make values result mismatch")
	}
}
