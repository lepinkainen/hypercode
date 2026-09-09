// Command checkformat checks Go formatting without rewriting source files.
package main

import (
	"fmt"
	"os"
	"os/exec"
)

func main() {
	out, err := exec.Command("gofmt", "-l", "cmd", "internal").CombinedOutput()
	if err != nil || len(out) != 0 {
		fmt.Printf("Run gofmt on the following files:\n%s", out)
		os.Exit(1)
	}
}
