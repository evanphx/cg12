package main

type mapStructValue struct {
	name   string
	values []int
}

func main() {
	items := map[string]mapStructValue{
		"item": {
			name:   "old",
			values: []int{1, 2},
		},
	}

	item := items["item"]
	item.name = "new"
	item.values = append(item.values, 3, 4)
	items["item"] = item

	reloaded := items["item"]
	if reloaded.name != "new" || len(reloaded.values) != 4 {
		panic("map struct value replacement failed")
	}

	total := 0
	for _, value := range reloaded.values {
		total += value
	}
	if total != 10 {
		panic("map struct value replacement corrupted slice")
	}
}
