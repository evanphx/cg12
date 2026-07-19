package main

func main() {
	var values map[string]int
	if values["missing"] != 0 {
		panic("nil map read returned non-zero")
	}
	if _, ok := values["missing"]; ok {
		panic("nil map lookup reported present key")
	}

	delete(values, "missing")

	defer func() {
		recovered := recover()
		if recovered == nil {
			panic("nil map assignment did not panic")
		}
	}()
	values["missing"] = 1
}
