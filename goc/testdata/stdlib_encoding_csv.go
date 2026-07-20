package main

import (
	"encoding/csv"
	"strings"
)

func main() {
	input := "name,value\nalpha,17\nbeta,25\n"
	reader := csv.NewReader(strings.NewReader(input))
	records, err := reader.ReadAll()
	if err != nil {
		panic("csv ReadAll failed")
	}
	if len(records) != 3 || records[1][0] != "alpha" || records[2][1] != "25" {
		panic("csv records were parsed incorrectly")
	}

	var builder strings.Builder
	writer := csv.NewWriter(&builder)
	if err := writer.WriteAll(records); err != nil {
		panic("csv WriteAll failed")
	}
	if writer.Error() != nil {
		panic("csv writer recorded an error")
	}
	if builder.String() != input {
		panic("csv writer produced wrong output")
	}
}
