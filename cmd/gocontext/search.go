package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/ToledoVitor/GoContext/internal/ingest/localstore"
	searchdomain "github.com/ToledoVitor/GoContext/internal/search"
	"github.com/ToledoVitor/GoContext/internal/search/lexical"
)

const defaultSearchLimit = 10

func runSearch(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("search", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "uso: gocontext search [--store DIR] [--limit N] <repositório> <consulta...>")
	}
	storeFlag := flags.String("store", "", "diretório local de snapshots")
	limit := flags.Int("limit", defaultSearchLimit, "máximo de resultados")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() < 2 || *limit <= 0 {
		flags.Usage()
		return 2
	}

	storePath, err := storeDirectory(*storeFlag)
	if err != nil {
		fmt.Fprintf(stderr, "consultar repositório: %v\n", err)
		return 1
	}
	repositoryID, err := canonicalRepositoryPath(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "consultar repositório: %v\n", err)
		return 1
	}
	store, err := localstore.NewStore(storePath)
	if err != nil {
		fmt.Fprintf(stderr, "consultar repositório: %v\n", err)
		return 1
	}
	searcher, err := lexical.NewSearcher(store)
	if err != nil {
		fmt.Fprintf(stderr, "consultar repositório: %v\n", err)
		return 1
	}

	hits, err := searcher.Search(ctx, searchdomain.Query{
		RepositoryID: repositoryID,
		Text:         strings.Join(flags.Args()[1:], " "),
		Limit:        *limit,
	})
	if err != nil {
		fmt.Fprintf(stderr, "consultar repositório: %v\n", err)
		return 1
	}
	if len(hits) == 0 {
		fmt.Fprintln(stdout, "nenhum resultado")
		return 0
	}

	for index, hit := range hits {
		if index > 0 {
			fmt.Fprintln(stdout)
		}
		reference := hit.Chunk.Reference
		fmt.Fprintf(
			stdout,
			"%.3f %s:%d-%d",
			hit.Score,
			safeTerminalValue(reference.Path, false),
			reference.StartLine,
			reference.EndLine,
		)
		if hit.Chunk.SymbolName != "" {
			fmt.Fprintf(stdout, " %s", safeTerminalValue(hit.Chunk.SymbolName, false))
		}
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, safeTerminalValue(hit.Chunk.Text, true))
	}
	return 0
}

func safeTerminalValue(value string, multiline bool) string {
	var safe strings.Builder
	for _, character := range value {
		if multiline && (character == '\n' || character == '\t') {
			safe.WriteRune(character)
			continue
		}
		if !unicode.IsControl(character) {
			safe.WriteRune(character)
			continue
		}

		switch {
		case character <= 0xff:
			fmt.Fprintf(&safe, `\x%02x`, character)
		case character <= 0xffff:
			fmt.Fprintf(&safe, `\u%04x`, character)
		default:
			fmt.Fprintf(&safe, `\U%08x`, character)
		}
	}
	return safe.String()
}
