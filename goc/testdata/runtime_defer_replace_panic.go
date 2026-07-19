package main

func main() {
	value := replacePanic()
	if value != "replacement" {
		panic("defer did not replace panic")
	}
}

func replacePanic() (value string) {
	defer func() {
		recovered := recover()
		if recovered != "replacement" {
			value = "wrong"
			return
		}
		value = recovered.(string)
	}()
	defer func() {
		panic("replacement")
	}()
	panic("original")
}
