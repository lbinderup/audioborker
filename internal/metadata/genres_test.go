package metadata

import (
	"reflect"
	"testing"
)

// lockePaths is Audible's live category ladders for The Lies of Locke Lamora
// (B004K50434, 2026-10): two non-fiction ladders on a fantasy novel.
var lockePaths = [][]string{
	{"Relationships, Parenting & Personal Development", "Parenting & Families"},
	{"Relationships, Parenting & Personal Development", "Relationships"},
	{"Science Fiction & Fantasy", "Fantasy", "Epic"},
	{"Science Fiction & Fantasy", "Fantasy", "Humorous"},
	{"Science Fiction & Fantasy", "Fantasy", "Paranormal & Urban", "Urban"},
}

func TestCleanGenres(t *testing.T) {
	cases := []struct {
		name      string
		book      Book
		genres    []string
		subGenres []string
	}{
		{"miscategorized fiction, labelled",
			Book{LiteratureType: "fiction", GenrePaths: lockePaths},
			[]string{"Science Fiction & Fantasy"},
			[]string{"Fantasy", "Epic", "Humorous", "Paranormal & Urban", "Urban"}},
		{"miscategorized fiction, unlabelled: the ladder majority decides",
			Book{GenrePaths: lockePaths},
			[]string{"Science Fiction & Fantasy"},
			[]string{"Fantasy", "Epic", "Humorous", "Paranormal & Urban", "Urban"}},
		{"unlabelled tie keeps both",
			Book{GenrePaths: [][]string{{"History", "Europe"}, {"Literature & Fiction", "Historical Fiction"}}},
			[]string{"History", "Literature & Fiction"},
			[]string{"Europe", "Historical Fiction"}},
		{"non-fiction drops fiction categories",
			Book{LiteratureType: "nonfiction", GenrePaths: [][]string{{"History", "Europe"}, {"Literature & Fiction", "Classics"}}},
			[]string{"History"},
			[]string{"Europe"}},
		{"best supported genre first",
			Book{LiteratureType: "fiction", GenrePaths: [][]string{
				{"Comedy & Humor", "Literature & Fiction"},
				{"Science Fiction & Fantasy", "Fantasy", "Epic"},
				{"Science Fiction & Fantasy", "Fantasy", "Humorous"},
			}},
			[]string{"Science Fiction & Fantasy", "Comedy & Humor"},
			[]string{"Fantasy", "Epic", "Humorous", "Literature & Fiction"}},
		{"a label contradicting every genre loses",
			Book{LiteratureType: "nonfiction", GenrePaths: [][]string{{"Science Fiction & Fantasy", "Fantasy"}}},
			[]string{"Science Fiction & Fantasy"},
			[]string{"Fantasy"}},
		{"unclassified names (other storefronts) pass through",
			Book{LiteratureType: "fiction", GenrePaths: [][]string{{"Science-Fiction & Fantasy", "Fantasy"}, {"Ratgeber", "Familie"}}},
			[]string{"Science-Fiction & Fantasy", "Ratgeber"},
			[]string{"Fantasy", "Familie"}},
		{"flat list: a dropped genre takes the unattributable sub-genres with it",
			Book{LiteratureType: "fiction",
				Genres:    []string{"Relationships, Parenting & Personal Development", "Science Fiction & Fantasy"},
				SubGenres: []string{"Parenting & Families", "Fantasy"}},
			[]string{"Science Fiction & Fantasy"},
			nil},
		{"flat list with nothing to drop keeps its sub-genres",
			Book{LiteratureType: "fiction", Genres: []string{"Science Fiction & Fantasy"}, SubGenres: []string{"Fantasy", "Epic"}},
			[]string{"Science Fiction & Fantasy"},
			[]string{"Fantasy", "Epic"}},
		{"flat list without a label is left alone",
			Book{Genres: []string{"Relationships, Parenting & Personal Development", "Science Fiction & Fantasy"}, SubGenres: []string{"Fantasy"}},
			[]string{"Relationships, Parenting & Personal Development", "Science Fiction & Fantasy"},
			[]string{"Fantasy"}},
		{"nothing at all",
			Book{LiteratureType: "fiction"},
			nil, nil},
	}
	for _, c := range cases {
		g, s := CleanGenres(&c.book)
		if !reflect.DeepEqual(g, c.genres) || !reflect.DeepEqual(s, c.subGenres) {
			t.Errorf("%s:\n got  %q / %q\n want %q / %q", c.name, g, s, c.genres, c.subGenres)
		}
	}
}

func TestGenreKind(t *testing.T) {
	for name, want := range map[string]string{
		"Science Fiction & Fantasy":                       KindFiction,
		" science fiction & fantasy ":                     KindFiction,
		"Relationships, Parenting & Personal Development": KindNonfiction,
		"Comedy & Humor":                                  "",
		"Teen & Young Adult":                              "",
		"Something New":                                   "",
	} {
		if got := GenreKind(name); got != want {
			t.Errorf("GenreKind(%q) = %q, want %q", name, got, want)
		}
	}
}
