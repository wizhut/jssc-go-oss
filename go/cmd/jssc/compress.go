package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wizhut/jssc-go-oss/go/sdk"
)

func newCompressCmd() *cobra.Command {
	var (
		algo      string
		structure string
		format    string
		maxDocs   int
	)

	cmd := &cobra.Command{
		Use:   "compress <file>",
		Short: "Compress JSON, JSONL, or NDJSON to <file>.jssc",
		Long: `Compress a document into a JSSC archive.

An archive holds a unique string pool, each entry compressed on its own, plus a
binary structure in which every string is a 1-, 2-, or 4-byte pool index.

Declare the input form with --format; the tool never guesses from the filename
or the bytes:

  json    a single JSON value        -> one document
  jsonl   newline-delimited JSON     -> one document per line, one shared pool
  ndjson  same wire form as jsonl, label preserved for round-trip

Structure blocks are lz4-compressed by default, so a cold query decompresses
one block rather than the structure. Use --structure deflate for the smallest
archive when it is headed to cold storage, or --structure raw to leave the
structure traversable with no decompression at all.

JSONL and NDJSON files larger than 64 MiB are encoded in two streaming passes,
so the whole corpus never has to fit in memory.

Examples:
  jssc compress data.json
  jssc compress --format jsonl events.jsonl
  jssc compress -f ndjson --structure raw logs.ndjson
  jssc compress -f jsonl --max-docs 10000 huge.jsonl`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sc, err := sdk.ParseStructure(structure)
			if err != nil {
				return err
			}
			format, err := sdk.ParseFormat(format)
			if err != nil {
				return err
			}

			outPath, st, err := sdk.CompressFile(args[0], sdk.Options{
				Algorithm: algo,
				Structure: sc,
				Format:    format,
				MaxDocs:   maxDocs,
			})
			if err != nil {
				return err
			}

			structDesc := fmt.Sprintf("struct=%d", st.StructBytes)
			if st.StructCodec != sdk.StructureRaw {
				structDesc = fmt.Sprintf("struct=%d(%s from %d)", st.StructBytes, st.StructCodec, st.StructPlain)
			}
			fmt.Fprintf(cmd.ErrOrStderr(),
				"compressed [%s/%s]: %d -> %d bytes (%.2fx, saved %d, docs=%d, strings=%d, index=%dB, pool=%d, %s, state=%d)\n",
				st.Algorithm, st.ContentFormat, st.RawBytes, st.TotalBytes, st.Ratio(), st.RawBytes-st.TotalBytes,
				st.DocCount, st.UniqueStrings, st.IndexWidth, st.PoolBytes, structDesc, st.StateBytes)
			fmt.Fprintln(cmd.ErrOrStderr(), "wrote", outPath)
			return nil
		},
	}

	cmd.Flags().StringVarP(&algo, "algorithm", "a", sdk.AlgoFSST,
		"string-pool codec ("+strings.Join(availableAlgorithms(), "|")+")")
	cmd.Flags().StringVarP(&structure, "structure", "s", "lz4",
		"structure block codec (lz4|deflate|raw)")
	cmd.Flags().StringVarP(&format, "format", "f", "json",
		"input textual form (json|jsonl|ndjson)")
	cmd.Flags().IntVar(&maxDocs, "max-docs", 0,
		"encode only the first N documents of line-oriented input (0 = all)")
	return cmd
}

func newDecompressCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "decompress <file>.jssc",
		Short: "Restore a .jssc archive to its original text",
		Long: `Restore a JSSC archive.

The input must end with .jssc; the output is written to the same path with that
suffix removed. The textual form comes from the archive header, so a JSONL
archive restores newline-delimited documents whatever the filename says.

Examples:
  jssc decompress data.json.jssc      # writes data.json
  jssc decompress events.jsonl.jssc   # writes events.jsonl`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			outPath, st, err := sdk.DecompressFile(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(),
				"restored %d bytes (algo=%s, format=%s, docs=%d, struct=%s, strings=%d, index=%dB)\n",
				st.RawBytes, st.Algorithm, st.ContentFormat, st.DocCount, st.StructCodec,
				st.UniqueStrings, st.IndexWidth)
			fmt.Fprintln(cmd.ErrOrStderr(), "wrote", outPath)
			return nil
		},
	}
}

func newInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info <file>.jssc",
		Short: "Print an archive's header without decoding it",
		Long: `Report what an archive contains — version, codecs, layout, pool size — by
reading its header and framing only. No pool entry is decompressed and the
structure is never walked.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			st, err := sdk.Inspect(raw)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "file            %s\n", args[0])
			fmt.Fprintf(out, "version         JSS%d\n", st.Version)
			fmt.Fprintf(out, "pool codec      %s\n", st.Algorithm)
			fmt.Fprintf(out, "struct codec    %s\n", st.StructCodec)
			fmt.Fprintf(out, "struct layout   %s\n", st.StructLayout)
			fmt.Fprintf(out, "content format  %s\n", st.ContentFormat)
			fmt.Fprintf(out, "documents       %d\n", st.DocCount)
			fmt.Fprintf(out, "index width     %d byte(s)\n", st.IndexWidth)
			if st.DistinctKeys > 0 || st.KeyWidth > 0 {
				fmt.Fprintf(out, "key width       %d byte(s)\n", st.KeyWidth)
				fmt.Fprintf(out, "key pool        %d entries, %d bytes\n", st.DistinctKeys, st.KeyPoolBytes)
			}
			fmt.Fprintf(out, "pool entries    %d\n", st.UniqueStrings)
			fmt.Fprintf(out, "header bytes    %d\n", st.HeaderBytes)
			fmt.Fprintf(out, "state bytes     %d\n", st.StateBytes)
			fmt.Fprintf(out, "pool bytes      %d\n", st.PoolBytes)
			if st.Version >= 6 {
				fmt.Fprintf(out, "key directory   %d keys, %d bytes", st.KeyDirEntries, st.KeyDirBytes)
				if st.KeyDirEntries == 0 {
					fmt.Fprint(out, " (absent: field lookup searches the pool)")
				}
				fmt.Fprintln(out)
			}
			fmt.Fprintf(out, "structure bytes %d stored", st.StructBytes)
			if st.StructCodec != sdk.StructureRaw {
				fmt.Fprintf(out, ", %d inflated", st.StructPlain)
			}
			fmt.Fprintln(out)
			fmt.Fprintf(out, "archive bytes   %d\n", len(raw))
			return nil
		},
	}
}

func availableAlgorithms() []string {
	var names []string
	for _, a := range sdk.Algorithms() {
		if a.Available {
			names = append(names, a.Name)
		}
	}
	return names
}
