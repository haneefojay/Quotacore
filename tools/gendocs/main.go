// Command gendocs renders api/openapi.yaml into the single self-contained HTML page that the
// service serves at /docs.
//
// It exists as its own command rather than as a Makefile recipe so that the same code path
// produces the page in three places: `make docs` for a developer, CI's drift check for a
// reviewer, and TestDocsPageIsUpToDate for the test suite. A generator that exists in three
// places as three shell snippets is three generators, and the two that are never run in anger are
// the two that are wrong.
//
// The -check flag is what CI uses. It writes nothing and exits non-zero when the committed page
// differs from the generated one, which is the property that makes the page trustworthy: a
// document that rots silently is a document nobody reads.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/quotacore/quotacore/api"
)

func main() {
	var (
		source   = flag.String("in", filepath.Join("api", "openapi.yaml"), "the contract to render")
		output   = flag.String("out", filepath.Join("api", "docs.html"), "the page to write")
		check    = flag.Bool("check", false, "write nothing; exit non-zero if the page is out of date")
		quietOut = flag.Bool("quiet", false, "write the page without announcing it")
	)
	flag.Parse()

	document, err := os.ReadFile(*source)
	if err != nil {
		fail("read %s: %v", *source, err)
	}
	page, err := api.RenderDocs(document)
	if err != nil {
		fail("render %s: %v", *source, err)
	}

	if *check {
		committed, readErr := os.ReadFile(*output)
		if readErr != nil {
			fail("%s is missing, so the page was never generated; run `make docs`", *output)
		}
		if !bytes.Equal(committed, page) {
			fail("%s differs from the page generated from %s; run `make docs` and commit the result",
				*output, *source)
		}
		fmt.Printf("%s is up to date with %s (%d bytes)\n", *output, *source, len(page))
		return
	}

	if err := os.WriteFile(*output, page, 0o644); err != nil {
		fail("write %s: %v", *output, err)
	}
	if !*quietOut {
		fmt.Printf("wrote %s (%d bytes) from %s\n", *output, len(page), *source)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gendocs: "+format+"\n", args...)
	os.Exit(1)
}
