package main

import (
	"bytes"
	"math/rand"
	"reflect"
	"slices"
	"testing/quick"
)

type boundedInt int

func (boundedInt) Generate(random *rand.Rand, size int) reflect.Value {
	return reflect.ValueOf(boundedInt(random.Intn(size + 1)))
}

type generatedInput struct {
	Count boundedInt
	Text  string
	Data  []byte
}

func main() {
	property := func(input generatedInput) bool {
		if input.Count < 0 || input.Count > 50 {
			return false
		}

		textCopy := []byte(input.Text)
		if string(textCopy) != input.Text {
			return false
		}

		dataCopy := append([]byte(nil), input.Data...)
		slices.Reverse(dataCopy)
		slices.Reverse(dataCopy)
		return bytes.Equal(dataCopy, input.Data)
	}
	configuration := &quick.Config{
		MaxCount: 64,
		Rand:     rand.New(rand.NewSource(42)),
	}
	if err := quick.Check(property, configuration); err != nil {
		panic(err)
	}
}
