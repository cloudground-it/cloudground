// Command gendoc writes docs/api.md from the API's route table:
//
//	go generate ./internal/api
//
// TestAPIDocIsCurrent fails while the committed file differs from its output.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/cloudground-it/cloudground/internal/api"
)

func main() {
	out := flag.String("o", "docs/api.md", "file to write")
	flag.Parse()
	doc, err := api.Reference()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gendoc:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, doc, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gendoc:", err)
		os.Exit(1)
	}
}
