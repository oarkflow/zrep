package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/zrep/zrep/internal/testdatafixture"
)

func main() {
	root := flag.String("root", "", "directory where testdata should be generated")
	rows := flag.Int("rows", 10000, "rows per generated structured/log file")
	files := flag.Int("files", 16, "large-file multiplier")
	days := flag.Int("days", 14, "number of daily log folders")
	matchEvery := flag.Int("match-every", 97, "write the marker every N rows")
	flag.Parse()
	if *root == "" {
		fmt.Fprintln(os.Stderr, "zrep-testdata: -root is required")
		os.Exit(2)
	}
	fx, err := testdatafixture.Stress(*root, testdatafixture.Options{
		Rows:       *rows,
		Files:      *files,
		Days:       *days,
		MatchEvery: *matchEvery,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "zrep-testdata: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(testdatafixture.FormatManifest(fx))
}
