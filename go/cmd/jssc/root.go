package main

import (
	"github.com/spf13/cobra"

	"github.com/wizhut/jssc-go-oss/go/sdk"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "jssc",
		Short: "JSON String-pool Compressed archives",
		Long: `jssc compresses JSON and newline-delimited JSON by pooling every distinct
string, compressing each pool entry with a short-string codec, and encoding the
value tree as a compact binary structure of tags and pool indices.

Archives are self-contained: no schema, no shared dictionary. Because the
structure carries length prefixes and child offset tables, a JSONPath query can
walk an archive and decode only the strings that path actually touches.`,
		Version:       sdk.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate(
		"jssc {{.Version}} (spec " + sdk.SpecVersion + ")\n")

	root.AddCommand(
		newCompressCmd(),
		newDecompressCmd(),
		newQueryCmd(),
		newInfoCmd(),
		newAlgorithmsCmd(),
	)
	return root
}
