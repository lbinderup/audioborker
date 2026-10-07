// Package parity pins the behaviour the Plex agent (plex/Audioborker.bundle)
// ports from this codebase: query normalization, candidate scoring and
// auto-selection, embedded-tag interpretation, HTML flattening, catalog
// response parsing and the field-by-field metadata merge.
//
// The test runs the real Go implementations over a fixed set of cases and
// compares the results with testdata/golden.json; plex/tests/test_parity.py
// checks the Python port against the same file. A behaviour change on either
// side therefore fails a test until both agree again:
//
//	go test ./internal/parity -update   # Go changed on purpose: re-record
//	cd plex && python -m unittest       # then make the port match
package parity
