package metadata

import (
	"sort"
	"strings"
)

// Audible files some books under categories that contradict what they are:
// The Lies of Locke Lamora sits in "Relationships, Parenting & Personal
// Development" beside Science Fiction & Fantasy, and Audnexus copies that.
// No free source is cleaner (Google Books needs a key, Open Library calls the
// same book juvenile fiction), so genres are cleaned with what the catalogs
// do provide: Audnexus' fiction/nonfiction label and the shape of Audible's
// category ladders.

const (
	KindFiction    = "fiction"
	KindNonfiction = "nonfiction"
)

// genreKinds classifies Audible's top-level categories as the English-language
// stores name them. Categories holding both kinds (Children's Audiobooks,
// Comedy & Humor, LGBTQ+, Teen & Young Adult) and names from other stores are
// absent on purpose: an unclassified genre is never dropped.
var genreKinds = map[string]string{
	"literature & fiction":         KindFiction,
	"science fiction & fantasy":    KindFiction,
	"mystery, thriller & suspense": KindFiction,
	"romance":                      KindFiction,
	"erotica":                      KindFiction,

	"arts & entertainment":       KindNonfiction,
	"biographies & memoirs":      KindNonfiction,
	"business & careers":         KindNonfiction,
	"computers & technology":     KindNonfiction,
	"education & learning":       KindNonfiction,
	"health & wellness":          KindNonfiction,
	"history":                    KindNonfiction,
	"home & garden":              KindNonfiction,
	"money & finance":            KindNonfiction,
	"politics & social sciences": KindNonfiction,
	"relationships, parenting & personal development": KindNonfiction,
	"religion & spirituality":                         KindNonfiction,
	"science & engineering":                           KindNonfiction,
	"sports & outdoors":                               KindNonfiction,
	"travel & tourism":                                KindNonfiction,
}

// GenreKind classifies a top-level genre name: KindFiction, KindNonfiction,
// or "" when it holds both or is unknown.
func GenreKind(name string) string {
	return genreKinds[strings.ToLower(strings.TrimSpace(name))]
}

// LiteratureKind is the book's kind: Audnexus' label when it has one,
// otherwise the majority of the catalog's category paths. "" when unknown or
// tied, which disables genre filtering.
func LiteratureKind(b *Book) string {
	switch strings.ToLower(strings.TrimSpace(b.LiteratureType)) {
	case KindFiction:
		return KindFiction
	case KindNonfiction:
		return KindNonfiction
	}
	fiction, nonfiction := 0, 0
	for _, p := range b.GenrePaths {
		if len(p) == 0 {
			continue
		}
		switch GenreKind(p[0]) {
		case KindFiction:
			fiction++
		case KindNonfiction:
			nonfiction++
		}
	}
	switch {
	case fiction > nonfiction:
		return KindFiction
	case nonfiction > fiction:
		return KindNonfiction
	}
	return ""
}

// CleanGenres returns the book's genres and sub-genres with every category
// that contradicts its kind removed, the best-supported genre first (it is
// the one written into the file).
//
// With category paths a sub-genre survives only under a surviving genre, so
// "Parenting & Families" leaves with its parent. Without them (Audnexus' flat
// list) sub-genres can't be attributed, so they are dropped whenever a genre
// is. Filtering never empties the list: if every genre contradicts the label,
// the label is the likelier mistake and all of them stay.
func CleanGenres(b *Book) (genres, subGenres []string) {
	kind := LiteratureKind(b)
	contradicts := func(name string) bool {
		k := GenreKind(name)
		return kind != "" && k != "" && k != kind
	}

	if len(b.GenrePaths) == 0 {
		var kept []string
		for _, g := range b.Genres {
			if !contradicts(g) {
				kept = append(kept, g)
			}
		}
		switch {
		case len(kept) == 0:
			return clone(b.Genres), clone(b.SubGenres)
		case len(kept) < len(b.Genres):
			return kept, nil
		}
		return kept, clone(b.SubGenres)
	}

	type genre struct {
		name    string
		support int
	}
	var order []*genre
	byName := map[string]*genre{}
	for _, p := range b.GenrePaths {
		if len(p) == 0 || p[0] == "" {
			continue
		}
		g := byName[p[0]]
		if g == nil {
			g = &genre{name: p[0]}
			byName[p[0]] = g
			order = append(order, g)
		}
		g.support++
	}
	var keep []*genre
	for _, g := range order {
		if !contradicts(g.name) {
			keep = append(keep, g)
		}
	}
	if len(keep) == 0 {
		keep = order
	}
	// Stable: equally supported genres keep the catalog's order.
	sort.SliceStable(keep, func(i, j int) bool { return keep[i].support > keep[j].support })

	seen := map[string]bool{}
	for _, g := range keep {
		genres = append(genres, g.name)
		seen[g.name] = true
	}
	for _, g := range keep {
		for _, p := range b.GenrePaths {
			if len(p) == 0 || p[0] != g.name {
				continue
			}
			for _, s := range p[1:] {
				if s != "" && !seen[s] {
					seen[s] = true
					subGenres = append(subGenres, s)
				}
			}
		}
	}
	return genres, subGenres
}

func clone(s []string) []string {
	if s == nil {
		return nil
	}
	return append([]string(nil), s...)
}
