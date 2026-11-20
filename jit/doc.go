// Package jit runs machine code that cg12 compiled inside this process.
//
// Load lays a relocatable object out in executable memory, and Call runs
// a function in it on a stack this package owns. Compiled code follows
// the platform's C ABI, never calls back into Go, and holds no Go
// pointers, so Go's scheduler and collector never meet it: a goroutine
// running compiled code just can't be preempted until it returns.
//
// linux/amd64 and linux/arm64 are supported; elsewhere Supported is
// false and Load fails.
package jit
