// Command jssc compresses, restores, inspects, and queries JSSC archives.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "jssc:", err)
		os.Exit(1)
	}
}
