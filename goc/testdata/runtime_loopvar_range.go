package main

import (
	"fmt"
	"sort"
)

// The iteration variables of every range form a range clause declares are
// per-iteration under Go 1.22, so closures created in the body observe that
// iteration's key and value rather than the last one.
func main() {
	var captured []func() string

	for index := range 3 {
		captured = append(captured, func() string { return fmt.Sprint(index) })
	}
	if values := collect(captured); values != "0 1 2" {
		panic("range over int shared one slot: " + values)
	}

	captured = nil
	for index, letter := range []string{"a", "b", "c"} {
		captured = append(captured, func() string { return fmt.Sprint(index) + letter })
	}
	if values := collect(captured); values != "0a 1b 2c" {
		panic("range over slice shared one slot: " + values)
	}

	captured = nil
	for index, value := range [3]int{7, 8, 9} {
		captured = append(captured, func() string { return fmt.Sprint(index, value) })
	}
	if values := collect(captured); values != "0 7 1 8 2 9" {
		panic("range over array shared one slot: " + values)
	}

	captured = nil
	for offset, letter := range "aéc" {
		captured = append(captured, func() string { return fmt.Sprint(offset) + string(letter) })
	}
	if values := collect(captured); values != "0a 1é 3c" {
		panic("range over string shared one slot: " + values)
	}

	captured = nil
	counts := map[string]int{"a": 1, "b": 2, "c": 3}
	for key, count := range counts {
		captured = append(captured, func() string { return key + fmt.Sprint(count) })
	}
	if values := sorted(captured); values != "a1 b2 c3" {
		panic("range over map shared one slot: " + values)
	}

	captured = nil
	stream := make(chan int, 3)
	stream <- 10
	stream <- 20
	stream <- 30
	close(stream)
	for value := range stream {
		captured = append(captured, func() string { return fmt.Sprint(value) })
	}
	if values := collect(captured); values != "10 20 30" {
		panic("range over channel shared one slot: " + values)
	}

	captured = nil
	for value := range counted {
		captured = append(captured, func() string { return fmt.Sprint(value) })
	}
	if values := collect(captured); values != "0 10 20" {
		panic("range over function iterator shared one slot: " + values)
	}

	captured = nil
	for index, name := range pairs {
		captured = append(captured, func() string { return fmt.Sprint(index) + name })
	}
	if values := collect(captured); values != "0x 1y 2z" {
		panic("two-value range over function iterator shared one slot: " + values)
	}
}

func counted(yield func(int) bool) {
	for index := 0; index < 3; index++ {
		if !yield(index * 10) {
			return
		}
	}
}

func pairs(yield func(int, string) bool) {
	for index, name := range []string{"x", "y", "z"} {
		if !yield(index, name) {
			return
		}
	}
}

func collect(closures []func() string) string {
	text := ""
	for _, closure := range closures {
		if text != "" {
			text += " "
		}
		text += closure()
	}
	return text
}

func sorted(closures []func() string) string {
	values := make([]string, 0, len(closures))
	for _, closure := range closures {
		values = append(values, closure())
	}
	sort.Strings(values)
	return collectStrings(values)
}

func collectStrings(values []string) string {
	text := ""
	for _, value := range values {
		if text != "" {
			text += " "
		}
		text += value
	}
	return text
}
