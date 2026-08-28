package main

import (
	"context"
	"fmt"
	"io"
	"os"
)

var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithSearchObserver(args, stdout, stderr, nil)
}

func runWithSearchObserver(args []string, stdout, stderr io.Writer, observer searchHitObserver) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Fprintf(stdout, "gocontext %s\n", version)
		return 0
	}
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	switch args[0] {
	case "index":
		return runIndex(context.Background(), args[1:], stdout, stderr)
	case "search":
		return runSearchWithObserver(context.Background(), args[1:], stdout, stderr, observer)
	case "help", "--help", "-h":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "comando desconhecido: %s\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func printUsage(writer io.Writer) {
	fmt.Fprintln(writer, "uso: gocontext <comando> [opções]")
	fmt.Fprintln(writer, "comandos:")
	fmt.Fprintln(writer, "  index    indexa repositório local")
	fmt.Fprintln(writer, "  search   consulta índice local")
}
