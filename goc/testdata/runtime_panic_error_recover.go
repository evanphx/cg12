package main

import "fmt"

type errorRecoverParser struct{}

func (p *errorRecoverParser) recoverError(errp *error) {
	value := recover()
	if value != nil {
		*errp = value.(error)
	}
}

func (p *errorRecoverParser) parse() (err error) {
	defer p.recoverError(&err)
	panic(fmt.Errorf("%s", "bad"))
}

func main() {
	var parser errorRecoverParser
	err := parser.parse()
	if err == nil {
		panic("missing recovered error")
	}
	if err.Error() != "bad" {
		panic("wrong recovered error")
	}
}
