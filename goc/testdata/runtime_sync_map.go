package main

import "sync"

func main() {
	var values sync.Map
	for index := 0; index < 32; index++ {
		values.Store(index, index*2)
	}

	values.Delete(3)
	values.Delete(9)

	total := 0
	count := 0
	values.Range(func(key any, value any) bool {
		total += key.(int)
		total += value.(int)
		count++
		return true
	})

	if count != 30 || total != 1452 {
		panic("sync.Map range result mismatch")
	}

	value, ok := values.Load(11)
	if !ok || value.(int) != 22 {
		panic("sync.Map load result mismatch")
	}
}
