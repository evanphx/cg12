package main

var initialized int

func init() {
	initialized = 42
}

func main() {
	if initialized != 42 {
		panic("init did not run")
	}
	println("hello from cg12 Go")
}
