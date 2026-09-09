// Command testcodex is an offline protocol peer for browser testing.
package main

import (
	"hypercode/internal/testcodex"
	"log"
	"os"
)

func main() {
	if err := testcodex.Run(os.Stdin, os.Stdout); err != nil {
		log.Fatal(err)
	}
}
