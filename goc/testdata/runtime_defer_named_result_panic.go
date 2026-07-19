package main

func computeNamedResult() (result int) {
	defer func() {
		if recover() != "replace result" {
			panic("wrong panic recovered")
		}
		result = 42
	}()
	result = 17
	panic("replace result")
}

func main() {
	if computeNamedResult() != 42 {
		panic("defer failed to replace named result after recover")
	}
}
