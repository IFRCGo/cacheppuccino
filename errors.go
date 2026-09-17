package main

import "errors"

// errEmptyImport marks an upstream file that parsed cleanly but held no
// servable rows. Replacing good data with nothing is never an improvement,
// so the import is rejected rather than applied.
var errEmptyImport = errors.New("import produced no servable rows")
