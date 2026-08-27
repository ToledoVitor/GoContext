package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("gocontext", flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "exibe a versão")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	if *showVersion {
		fmt.Fprintf(stdout, "gocontext %s\n", version)
		return 0
	}

	fmt.Fprintln(stdout, "GoContext: fundação pronta; indexação ainda não implementada")
	return 0
}
