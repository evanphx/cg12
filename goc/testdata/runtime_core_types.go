package main

type counter interface {
	Value() int
}

type bucket struct {
	name   string
	values []int
}

func (item *bucket) Value() int {
	total := 0
	for _, value := range item.values {
		total += value
	}
	return total
}

func main() {
	buckets := map[string]*bucket{
		"left": {
			name:   "left",
			values: []int{7, 11},
		},
		"right": {
			name:   "right",
			values: []int{13},
		},
	}

	buckets["right"].values = append(buckets["right"].values, 17)

	var current counter = buckets["left"]
	total := current.Value()

	current = buckets["right"]
	total += current.Value()

	if total != 48 {
		panic("core type computation mismatch")
	}
}
