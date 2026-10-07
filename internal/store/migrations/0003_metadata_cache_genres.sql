-- Cached catalog records predate the genre hierarchy (Book.GenrePaths,
-- SubGenres, LiteratureType) that metadata.CleanGenres needs to drop
-- miscategorized genres. Blank the book records so they are fetched again;
-- chapter data is unaffected and stays. An empty book_json reads as a miss.
UPDATE metadata_cache SET book_json = '';
