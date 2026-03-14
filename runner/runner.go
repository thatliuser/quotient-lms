// Package main previously contained a standalone runner binary that consumed
// tasks from Redis and executed service checks. This has been replaced by
// in-process goroutine workers in the engine package (see engine/broker.go).
//
// The runner binary is no longer needed. Service checks are now executed
// directly in the server process via the engine's worker pool.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Println("The standalone runner binary has been removed.")
	fmt.Println("Service checks now run in-process within the server via goroutine workers.")
	fmt.Println("See engine/broker.go for the implementation.")
	os.Exit(1)
}
