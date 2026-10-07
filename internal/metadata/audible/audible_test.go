package audible

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"audioborker/internal/metadata"
)

// productJSON is a trimmed capture of the live
// /1.0/catalog/products/B003ZWFO7E response (2026-08). Field shapes must stay
// accurate to the real API — re-capture rather than hand-edit.
const productJSON = `{
  "product": {
    "asin": "B003ZWFO7E",
    "title": "The Way of Kings",
    "subtitle": "Book One of the Stormlight Archive",
    "authors": [{"asin": "B001IGFHW6", "name": "Brandon Sanderson"}],
    "narrators": [{"name": "Kate Reading"}, {"name": "Michael Kramer"}],
    "publisher_name": "Macmillan Audio",
    "publisher_summary": "<p><b>From #1 <i>New York Times</i> bestselling author Brandon Sanderson.</b><br /><br />Roshar is a world of stone &amp; storms.</p>",
    "merchandising_summary": "From #1 New York Times bestselling author Brandon Sanderson, an incredible new saga of epic proportion...",
    "release_date": "2010-08-31",
    "runtime_length_min": 2730,
    "language": "english",
    "product_images": {
      "1000": "https://m.media-amazon.com/images/I/91iPex6QO8L._SL1000_.jpg",
      "500": "https://m.media-amazon.com/images/I/51hAwcG3oNL._SL500_.jpg"
    },
    "series": [
      {"asin": "B006K1RP8I", "sequence": "1", "title": "The Stormlight Archive", "url": "/pd/The-Stormlight-Archive-Audiobook/B006K1RP8I"},
      {"asin": "B0DMXTJ8WH", "sequence": "", "title": "The Cosmere", "url": "/pd/The-Cosmere-Audiobook/B0DMXTJ8WH"}
    ],
    "category_ladders": [
      {"ladder": [{"id": "18580606011", "name": "Science Fiction & Fantasy"}, {"id": "18580607011", "name": "Fantasy"}, {"id": "18580608011", "name": "Action & Adventure"}], "root": "Genres"},
      {"ladder": [{"id": "18580606011", "name": "Science Fiction & Fantasy"}, {"id": "18580607011", "name": "Fantasy"}, {"id": "18580615011", "name": "Epic"}], "root": "Genres"}
    ]
  },
  "response_groups": ["product_desc", "always-returned", "product_extended_attrs", "contributors", "series", "media", "category_ladders", "product_attrs"]
}`

// emptyProductJSON is a real capture too: existing-but-unavailable ASINs
// return HTTP 200 with only the "always-returned" groups and no title.
const emptyProductJSON = `{"product":{"asin":"B00BWY9UM8","asset_details":[],"is_vvab":false},"response_groups":["always-returned","category_ladders"]}`

// detailClient points a Client at a test server. The region TLD is what picks
// the host in production, so tests rewrite the transport instead.
func detailClient(srv *httptest.Server) *Client {
	c := New("audioborker/test")
	c.http = srv.Client()
	c.http.Transport = rewriteHost{srv: srv}
	return c
}

type rewriteHost struct{ srv *httptest.Server }

func (t rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(t.srv.URL, "http://")
	return http.DefaultTransport.RoundTrip(req)
}

func TestGetBookParsesLiveShape(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Write([]byte(productJSON))
	}))
	defer srv.Close()

	c := detailClient(srv)
	book, err := c.GetBook(context.Background(), "B003ZWFO7E", "us")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/1.0/catalog/products/B003ZWFO7E" {
		t.Errorf("path = %s", gotPath)
	}
	if !strings.Contains(gotQuery, "series") || !strings.Contains(gotQuery, "product_extended_attrs") {
		t.Errorf("response_groups not requested: %s", gotQuery)
	}

	if book.Title != "The Way of Kings" || book.Subtitle != "Book One of the Stormlight Archive" {
		t.Errorf("title parse: %+v", book)
	}
	if book.Publisher != "Macmillan Audio" || book.Year != "2010" || book.ReleaseDate != "2010-08-31" {
		t.Errorf("publisher/date parse: %+v", book)
	}
	// The numbered series must win over the sequence-less umbrella collection.
	if book.SeriesName != "The Stormlight Archive" || book.SeriesPosition != "1" {
		t.Errorf("series = %q #%q", book.SeriesName, book.SeriesPosition)
	}
	if len(book.Authors) != 1 || book.Authors[0] != "Brandon Sanderson" {
		t.Errorf("authors: %v", book.Authors)
	}
	if len(book.Narrators) != 2 || book.Narrators[1] != "Michael Kramer" {
		t.Errorf("narrators: %v", book.Narrators)
	}
	// Hi-res image preferred; genres deduped to the ladders' first rung.
	if !strings.Contains(book.CoverURL, "_SL1000_") {
		t.Errorf("cover should prefer the 1000px image, got %s", book.CoverURL)
	}
	if len(book.Genres) != 1 || book.Genres[0] != "Science Fiction & Fantasy" {
		t.Errorf("genres: %v", book.Genres)
	}
	// HTML blurbs flattened for the mp4 atoms.
	wantSummary := "From #1 New York Times bestselling author Brandon Sanderson.\n\nRoshar is a world of stone & storms."
	if book.Summary != wantSummary {
		t.Errorf("summary =\n%q\nwant\n%q", book.Summary, wantSummary)
	}
	if strings.Contains(book.Description, "<") {
		t.Errorf("description not flattened: %q", book.Description)
	}
	if book.RuntimeMin != 2730 || book.Language != "english" {
		t.Errorf("attrs: %+v", book)
	}
}

func TestGetBookEmptyProductIsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(emptyProductJSON))
	}))
	defer srv.Close()

	_, err := detailClient(srv).GetBook(context.Background(), "B00BWY9UM8", "us")
	if !metadata.IsNotFound(err) {
		t.Fatalf("empty product should map to NotFound, got %v", err)
	}
}

func TestGetBook404IsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	_, err := detailClient(srv).GetBook(context.Background(), "B000000000", "us")
	if !metadata.IsNotFound(err) {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestGetBookUnknownRegion(t *testing.T) {
	if _, err := New("audioborker/test").GetBook(context.Background(), "B003ZWFO7E", "xx"); err == nil {
		t.Fatal("unknown region accepted")
	}
}

func TestGetBookRegionPicksTLD(t *testing.T) {
	// No transport rewrite here: the request must fail to resolve, but the
	// URL construction is observable through the error text.
	c := New("audioborker/test")
	c.http.Timeout = 1 // effectively instant
	_, err := c.GetBook(context.Background(), "B003ZWFO7E", "de")
	if err == nil || !strings.Contains(err.Error(), "api.audible.de") {
		t.Errorf("expected api.audible.de in error, got %v", err)
	}
}

// searchWithPodcastJSON is trimmed from the live search for "The Last Colony
// John Scalzi" (2026-10), where a fan podcast episode ranked above the book.
const searchWithPodcastJSON = `{"products": [
  {"asin": "B002VA9CAQ", "title": "The Last Colony", "authors": [{"name": "John Scalzi"}],
   "content_type": "Product", "content_delivery_type": "MultiPartBook", "runtime_length_min": 591},
  {"asin": "B0FXK18PYR", "title": "The Last Colony - John Scalzi",
   "content_type": "Podcast", "content_delivery_type": "PodcastEpisode", "runtime_length_min": 591},
  {"asin": "B0BBCRADIO", "title": "A Radio Dramatization",
   "content_type": "Radio/TV Program", "content_delivery_type": "SinglePartBook"}
]}`

func TestSearchSkipsPodcasts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(searchWithPodcastJSON))
	}))
	defer srv.Close()
	rs, err := detailClient(srv).Search(context.Background(), metadata.SearchQuery{Keywords: "x", Region: "us"})
	if err != nil {
		t.Fatal(err)
	}
	var asins []string
	for _, r := range rs {
		asins = append(asins, r.ASIN)
	}
	if strings.Join(asins, ",") != "B002VA9CAQ,B0BBCRADIO" {
		t.Errorf("got %v, want the book and the dramatization but no podcast episode", asins)
	}
}
