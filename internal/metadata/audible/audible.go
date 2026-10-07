// Package audible searches Audible's public catalog API for ASIN candidates.
// The API needs no auth; this is the same route audiobookshelf and the Plex
// Audnexus agent use. It is unofficial, so failures here must degrade to
// manual ASIN entry, never block the app.
package audible

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"audioborker/internal/metadata"
)

// regionTLD maps Audnexus/Audible region codes to catalog API TLDs.
var regionTLD = map[string]string{
	"us": ".com", "ca": ".ca", "uk": ".co.uk", "au": ".com.au", "fr": ".fr",
	"de": ".de", "jp": ".co.jp", "it": ".it", "in": ".in", "es": ".es",
}

type Client struct {
	http      *http.Client
	userAgent string
}

func New(userAgent string) *Client {
	return &Client{
		http:      &http.Client{Timeout: 15 * time.Second},
		userAgent: userAgent,
	}
}

// product is the subset of the catalog response we consume.
type product struct {
	ASIN     string `json:"asin"`
	Title    string `json:"title"`
	Subtitle string `json:"subtitle"`
	Authors  []struct {
		Name string `json:"name"`
	} `json:"authors"`
	Narrators []struct {
		Name string `json:"name"`
	} `json:"narrators"`
	ReleaseDate   string            `json:"release_date"`
	RuntimeMin    int               `json:"runtime_length_min"`
	Language      string            `json:"language"`
	ProductImages map[string]string `json:"product_images"`
	// "Product" / "Podcast", and e.g. "MultiPartBook" / "PodcastEpisode".
	ContentType         string `json:"content_type"`
	ContentDeliveryType string `json:"content_delivery_type"`
}

// isPodcast reports whether a catalog product is podcast content. The search
// mixes podcast episodes in with books (a fan podcast titled "The Last Colony
// - John Scalzi" outranked the book itself), and Audnexus refuses them, so
// picking one could only end in a failed lookup.
func isPodcast(p product) bool {
	return strings.EqualFold(p.ContentType, "Podcast") ||
		strings.HasPrefix(strings.ToLower(p.ContentDeliveryType), "podcast")
}

// productDetail extends the search subset with fields the single-product
// endpoint returns when asked for the extra response groups.
type productDetail struct {
	product
	PublisherName        string `json:"publisher_name"`
	PublisherSummary     string `json:"publisher_summary"`     // full blurb, HTML
	MerchandisingSummary string `json:"merchandising_summary"` // teaser, sometimes HTML
	Series               []struct {
		Title    string `json:"title"`
		Sequence string `json:"sequence"`
	} `json:"series"`
	CategoryLadders []struct {
		Ladder []struct {
			Name string `json:"name"`
		} `json:"ladder"`
	} `json:"category_ladders"`
}

// Search queries the catalog. Empty query fields are omitted.
func (c *Client) Search(ctx context.Context, q metadata.SearchQuery) ([]metadata.SearchResult, error) {
	tld, ok := regionTLD[q.Region]
	if !ok {
		return nil, fmt.Errorf("unknown region %q", q.Region)
	}
	params := url.Values{
		"num_results":      {"25"},
		"products_sort_by": {"Relevance"},
		"response_groups":  {"contributors,product_desc,media,product_attrs"},
		"image_sizes":      {"500"},
	}
	if q.Keywords != "" {
		params.Set("keywords", q.Keywords)
	}
	if q.Title != "" {
		params.Set("title", q.Title)
	}
	if q.Author != "" {
		params.Set("author", q.Author)
	}

	u := fmt.Sprintf("https://api.audible%s/1.0/catalog/products?%s", tld, params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("audible search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("audible search: HTTP %d", resp.StatusCode)
	}

	var body struct {
		Products []product `json:"products"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("audible search: decode: %w", err)
	}

	out := make([]metadata.SearchResult, 0, len(body.Products))
	for _, p := range body.Products {
		if p.ASIN == "" || p.Title == "" || isPodcast(p) {
			continue
		}
		r := metadata.SearchResult{
			ASIN:       p.ASIN,
			Region:     q.Region,
			Title:      p.Title,
			Subtitle:   p.Subtitle,
			Year:       yearOf(p.ReleaseDate),
			Language:   p.Language,
			RuntimeMin: p.RuntimeMin,
			CoverURL:   p.ProductImages["500"],
		}
		for _, a := range p.Authors {
			r.Authors = append(r.Authors, a.Name)
		}
		for _, n := range p.Narrators {
			r.Narrators = append(r.Narrators, n.Name)
		}
		out = append(out, r)
	}
	return out, nil
}

// GetBook fetches one catalog product. Notably contributes fields Audnexus
// sometimes lacks: the series list (Audnexus exposes only one primary series)
// and a second, often fresher publisher blurb. Per this package's contract,
// callers must treat failures as "source unavailable", never as fatal. A 404
// — and a 200 whose product carries no title, which is what non-audiobook or
// region-restricted ASINs return — maps to metadata.NotFound.
func (c *Client) GetBook(ctx context.Context, asin, region string) (*metadata.Book, error) {
	tld, ok := regionTLD[region]
	if !ok {
		return nil, fmt.Errorf("unknown region %q", region)
	}
	params := url.Values{
		"response_groups": {"contributors,product_desc,media,product_attrs,product_extended_attrs,series,category_ladders"},
		"image_sizes":     {"500,1000"},
	}
	u := fmt.Sprintf("https://api.audible%s/1.0/catalog/products/%s?%s", tld, url.PathEscape(asin), params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("audible product: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, metadata.NotFound("not in Audible's " + region + " catalog")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("audible product: HTTP %d", resp.StatusCode)
	}

	var body struct {
		Product productDetail `json:"product"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("audible product: decode: %w", err)
	}
	p := body.Product
	if p.ASIN == "" || p.Title == "" {
		return nil, metadata.NotFound("Audible has no audiobook data for this ASIN in region " + region)
	}

	book := &metadata.Book{
		ASIN:        p.ASIN,
		Region:      region,
		Title:       p.Title,
		Subtitle:    p.Subtitle,
		Publisher:   p.PublisherName,
		Language:    p.Language,
		Description: metadata.HTMLToText(p.MerchandisingSummary),
		Summary:     metadata.HTMLToText(p.PublisherSummary),
		RuntimeMin:  p.RuntimeMin,
		Year:        yearOf(p.ReleaseDate),
	}
	if len(p.ReleaseDate) >= 10 {
		book.ReleaseDate = p.ReleaseDate[:10]
	}
	if book.CoverURL = p.ProductImages["1000"]; book.CoverURL == "" {
		book.CoverURL = p.ProductImages["500"]
	}
	for _, a := range p.Authors {
		book.Authors = append(book.Authors, a.Name)
	}
	for _, n := range p.Narrators {
		book.Narrators = append(book.Narrators, n.Name)
	}
	// The series list can carry umbrella collections with no sequence (e.g.
	// "The Cosmere" alongside "The Stormlight Archive") — prefer the first
	// numbered entry, which matches what Audnexus calls the primary series.
	for _, s := range p.Series {
		if s.Sequence != "" {
			book.SeriesName, book.SeriesPosition = s.Title, s.Sequence
			break
		}
	}
	if book.SeriesName == "" && len(p.Series) > 0 {
		book.SeriesName = p.Series[0].Title
	}
	// Each category ladder is a path like Science Fiction & Fantasy → Fantasy
	// → Epic: the first rung is the genre, the rest are sub-genres. Ladders
	// repeat rungs, so dedupe preserving order. The paths themselves are kept
	// raw: metadata.CleanGenres needs them to drop a miscategorized genre
	// together with its sub-genres.
	seenGenre, seenSub := map[string]bool{}, map[string]bool{}
	for _, cl := range p.CategoryLadders {
		if len(cl.Ladder) == 0 {
			continue
		}
		path := make([]string, 0, len(cl.Ladder))
		for _, rung := range cl.Ladder {
			path = append(path, rung.Name)
		}
		book.GenrePaths = append(book.GenrePaths, path)
		if !seenGenre[path[0]] {
			seenGenre[path[0]] = true
			book.Genres = append(book.Genres, path[0])
		}
		for _, s := range path[1:] {
			if !seenSub[s] {
				seenSub[s] = true
				book.SubGenres = append(book.SubGenres, s)
			}
		}
	}
	return book, nil
}

func yearOf(date string) string {
	if len(date) >= 4 {
		return strings.SplitN(date, "-", 2)[0]
	}
	return ""
}
