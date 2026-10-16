// webpdffixture writes a deterministic text-layer PDF, one argument per page.
package main

import (
	"os"

	"github.com/edu-agent/edu-agent/server/internal/pdffixture"
)

func main() {
	if _, err := os.Stdout.Write(pdffixture.Build(os.Args[1:]...)); err != nil {
		panic(err)
	}
}
