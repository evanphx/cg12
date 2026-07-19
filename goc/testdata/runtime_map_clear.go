package main

func main() {
	counts := map[string]int{
		"alpha": 1,
		"beta":  2,
		"gamma": 3,
	}
	clear(counts)
	if len(counts) != 0 {
		panic("map clear kept entries")
	}

	counts["delta"] = 4
	counts["epsilon"] = 5
	if len(counts) != 2 || counts["delta"]+counts["epsilon"] != 9 {
		panic("map clear broke reuse")
	}
}
