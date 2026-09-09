// Command testclaude is an offline Claude Code protocol peer for browser testing.
package main

import (
	"log"
	"os"

	"github.com/lepinkainen/hypercode/internal/testclaude"
)

func main() {
	if err := testclaude.Run(os.Stdin, os.Stdout, testclaude.SessionFromArgs(os.Args), testclaude.ModelFromArgs(os.Args)); err != nil {
		log.Fatal(err)
	}
}
