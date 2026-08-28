package main

import (
	"errors"
	"flag"

	searchdomain "github.com/ToledoVitor/GoContext/internal/search"
	"github.com/ToledoVitor/GoContext/internal/source"
)

var errInvalidSearchFilter = errors.New("invalid search filter")

type searchFilterOptions struct {
	pathPrefixes []string
	languages    []string
}

func addSearchFilterFlags(flags *flag.FlagSet, options *searchFilterOptions) {
	flags.Func("path-prefix", "prefixo de caminho relativo; pode repetir", func(value string) error {
		options.pathPrefixes = append(options.pathPrefixes, value)
		return nil
	})
	flags.Func("language", "linguagem javascript|python|typescript; pode repetir", func(value string) error {
		options.languages = append(options.languages, value)
		return nil
	})
}

func resolveSearchFilter(options searchFilterOptions) (searchdomain.Filter, error) {
	filter := searchdomain.Filter{PathPrefixes: options.pathPrefixes}
	if options.languages != nil {
		filter.Languages = make([]source.Language, len(options.languages))
	}
	for position, language := range options.languages {
		switch language {
		case string(source.LanguageJavaScript):
			filter.Languages[position] = source.LanguageJavaScript
		case string(source.LanguagePython):
			filter.Languages[position] = source.LanguagePython
		case string(source.LanguageTypeScript):
			filter.Languages[position] = source.LanguageTypeScript
		default:
			return searchdomain.Filter{}, errInvalidSearchFilter
		}
	}
	if err := searchdomain.ValidateFilter(filter); err != nil {
		return searchdomain.Filter{}, errInvalidSearchFilter
	}
	return filter, nil
}
