// Package silkmath holds SILK's separate fixed-point dialect (silk/SigProc_FIX.h:
// silk_SMULWB and friends) as exact Go expressions with exhaustive tests against
// the C semantics. Kept distinct from fixedmath because SILK and CELT use
// different macro conventions. Deliberately C-shaped.
package silkmath
