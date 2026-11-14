package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/wizhut/jssc-go-oss/go/jssc"
	"github.com/wizhut/jssc-go-oss/go/sdk"
)

func newQueryCmd() *cobra.Command {
	var (
		pathsOnly  bool
		valuesOnly bool
		firstOnly  bool
		limit      int
		showStats  bool
	)

	cmd := &cobra.Command{
		Use:   "query <jsonpath> <file>",
		Short: "Evaluate a JSONPath against JSON or a .jssc archive",
		Long: `Evaluate a JSONPath expression and print the matches.

<file> may be JSON text or a JSSC archive; archives are recognized by their
magic bytes, not their name. An archive is queried by walking its binary
structure, so only the pool strings the path touches are ever decompressed.

Each match prints as "<path>\t<json value>". Use --paths or --values for one
side alone, --first for just the first value, or --limit to stop early.

Examples:
  jssc query '$.users[*].name' users.json
  jssc query '$..role' users.json.jssc
  jssc query --values '$.users[?(@.role=="admin")].name' users.json
  jssc query --first '$[0].actor.login' events.jsonl.jssc`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if pathsOnly && valuesOnly {
				return fmt.Errorf("use only one of --paths or --values")
			}
			path, input := args[0], args[1]
			raw, err := os.ReadFile(input)
			if err != nil {
				return err
			}
			isArchive := jssc.IsArchive(raw)
			out := cmd.OutOrStdout()

			if firstOnly {
				var (
					v  any
					ok bool
				)
				if isArchive {
					v, ok, err = sdk.QueryArchiveOne(raw, path)
				} else {
					v, ok, err = sdk.QueryOne(raw, path)
				}
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("no match for %q", path)
				}
				return writeJSONLine(out, v)
			}

			var (
				matches []sdk.Match
				archive *jssc.Archive
			)
			if isArchive {
				archive, err = sdk.OpenArchive(raw)
				if err != nil {
					return err
				}
				matches, err = archive.QueryLimit(path, limit)
			} else {
				matches, err = sdk.Query(raw, path)
				if err == nil && limit > 0 && len(matches) > limit {
					matches = matches[:limit]
				}
			}
			if err != nil {
				return err
			}

			for _, m := range matches {
				switch {
				case pathsOnly:
					fmt.Fprintln(out, m.Path)
				case valuesOnly:
					if err := writeJSONLine(out, m.Value); err != nil {
						return err
					}
				default:
					enc, err := json.Marshal(m.Value)
					if err != nil {
						return err
					}
					fmt.Fprintf(out, "%s\t%s\n", m.Path, enc)
				}
			}

			if showStats && archive != nil {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"%d match(es); decoded %d of %d pool strings, skipped %d values\n",
					len(matches), archive.DecodedStrings, archive.PoolCount(), archive.SkippedValues)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&pathsOnly, "paths", false, "print only normalized paths")
	cmd.Flags().BoolVar(&valuesOnly, "values", false, "print only matched values as JSON")
	cmd.Flags().BoolVar(&firstOnly, "first", false, "print only the first matched value")
	cmd.Flags().IntVar(&limit, "limit", 0, "stop after N matches (0 = all)")
	cmd.Flags().BoolVar(&showStats, "stats", false, "report pool decodes and skipped values (archives only)")
	return cmd
}

func writeJSONLine(w io.Writer, v any) error {
	enc, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(enc))
	return err
}

func newAlgorithmsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "algorithms",
		Short: "List the pool codecs and structure codecs",
		Long: `Print the codecs available to "compress --algorithm" and "compress --structure".

The pool codec (header field algo) compresses each string-pool entry on its
own; the structure codec (struct_codec) compresses the binary structure as a
whole. They are independent header fields.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "POOL CODEC\tID\tDEFAULT\tSTATUS\tDESCRIPTION")
			for _, a := range sdk.Algorithms() {
				status := "available"
				if !a.Available {
					status = "unavailable"
				}
				fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n", a.Name, a.ID, mark(a.Default), status, a.Description)
			}
			if err := w.Flush(); err != nil {
				return err
			}

			fmt.Fprintln(out)
			w = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "STRUCTURE CODEC\tID\tDEFAULT\tDESCRIPTION")
			for _, s := range sdk.Structures() {
				fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", s.Name, s.ID, mark(s.Default), s.Description)
			}
			if err := w.Flush(); err != nil {
				return err
			}

			return nil
		},
	}
}

func mark(b bool) string {
	if b {
		return "*"
	}
	return ""
}
