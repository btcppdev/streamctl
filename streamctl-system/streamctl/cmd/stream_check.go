package main

import (
	"context"
	"flag"
	"fmt"
	"streamctl/internal/probe"
)

type streamCheckFiles []string

func (f *streamCheckFiles) String() string         { return "" }
func (f *streamCheckFiles) Set(value string) error { *f = append(*f, value); return nil }
func runStreamCheck(args []string) error {
	flags := flag.NewFlagSet("stream-check", flag.ContinueOnError)
	var files streamCheckFiles
	flags.Var(&files, "file", "File to check; repeat for all playlist entries")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument: %s", flags.Arg(0))
	}
	return probe.ValidateStreamingPaths(context.Background(), files, files)
}
