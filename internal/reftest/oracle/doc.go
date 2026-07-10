// Package oracle wraps the pinned libopus (FIXED_POINT + DISABLE_FLOAT_API)
// as the differential-test oracle for go-opus.
//
// All of the cgo/libopus-touching code carries the `refc` build tag, so the
// normal pure-Go module build (`go build ./...`) ignores it entirely and this
// file is all that remains. See oracle_cgo.go for the encoder/decoder wrappers
// and shim.h/shim.c for the C surface. Build and run the oracle with:
//
//	go test -tags refc ./internal/reftest/...
package oracle
