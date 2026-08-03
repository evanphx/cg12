// Interface conversions that happen while a package-level initializer runs
// have to register their concrete type for dispatch, at every site an ordinary
// function body would.
//
// Each shape below uses a different interface and a different standard-library
// concrete type, so each one needs its own dispatch entry and no shape can pass
// on the back of another's registration. All seven are conversions the
// reachability walk sees only through a package-level `var` initializer: the
// site is a call argument, or it is inside a composite literal or a function
// literal that is itself part of an initializer expression.
//
// A missing entry is not a wrong answer, it is
//
//	cg12: interface dispatch failed for dynamic type 0x...
//
// at the first call through the interface, so reaching the end of main at all
// is most of what this program asserts.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"strings"
)

func passReader(r io.Reader) io.Reader { return r }

func firstWriter(writers ...io.Writer) io.Writer { return writers[0] }

func passStringer(s fmt.Stringer) fmt.Stringer { return s }

func passByteReader(r io.ByteReader) io.ByteReader { return r }

// A call argument, directly in a package-level variable initializer. This is
// the shape the miscompile was reduced to: *strings.Reader is converted to
// io.Reader nowhere else in the program.
var callArgument = passReader(strings.NewReader("A"))

// The same conversion through a variadic parameter.
var variadicArgument = firstWriter(new(bytes.Buffer))

// A call argument nested inside a composite literal at package scope.
var insideComposite = []fmt.Stringer{passStringer(fs.FileMode(0o644))}

// A call argument inside a function literal at package scope.
var insideFunctionLiteral = func() io.ByteReader {
	return passByteReader(bytes.NewReader([]byte("F")))
}

// An assignment inside a function literal at package scope.
var assignedInFunctionLiteral = func() io.ByteWriter {
	var writer io.ByteWriter
	writer = bufio.NewWriter(variadicArgument)
	return writer
}

// A return statement inside a function literal at package scope.
var returnedFromFunctionLiteral = func() io.StringWriter {
	return new(strings.Builder)
}

// A variable declaration inside a function literal at package scope.
var declaredInFunctionLiteral = func() io.RuneReader {
	var reader io.RuneReader = bufio.NewReader(callArgument)
	return reader
}

func main() {
	buffer := make([]byte, 1)
	if count, err := callArgument.Read(buffer); count != 1 || err != nil || buffer[0] != 'A' {
		panic("call argument in a package-level initializer")
	}

	if count, err := variadicArgument.Write([]byte("I")); count != 1 || err != nil {
		panic("variadic call argument in a package-level initializer")
	}

	if insideComposite[0].String() != "-rw-r--r--" {
		panic("call argument inside a package-scope composite literal")
	}

	if character, err := insideFunctionLiteral().ReadByte(); character != 'F' || err != nil {
		panic("call argument inside a package-scope function literal")
	}

	if err := assignedInFunctionLiteral().WriteByte('G'); err != nil {
		panic("assignment inside a package-scope function literal")
	}

	if count, err := returnedFromFunctionLiteral().WriteString("H"); count != 1 || err != nil {
		panic("return inside a package-scope function literal")
	}

	// callArgument has already given up its only byte, so this reports EOF --
	// which is an answer, and getting an answer at all is the point.
	if _, _, err := declaredInFunctionLiteral().ReadRune(); err != io.EOF {
		panic("variable declaration inside a package-scope function literal")
	}
}
