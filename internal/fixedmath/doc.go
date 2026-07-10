// Package fixedmath holds the CELT fixed-point macro helpers (celt/fixed_generic.h,
// mathops.h) as exact Go expressions, with exhaustive tests against the C
// semantics. It uses the OPUS_FAST_INT64 forms (docs/hard-parts.md 4). This
// package is deliberately C-shaped and never "simplified".
package fixedmath
