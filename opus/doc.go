// Package opus is the public API for the go-opus native Go Opus codec: a raw,
// buffer-based Encoder and Decoder (one PCM frame in, one packet out, and the
// reverse) plus packet-inspection helpers. It is the layer the differential
// conformance gates test. See docs/api-design.md.
//
// v1 scope is a CELT-only, 48 kHz, fixed-point encoder and a full RFC 6716
// decoder. The container layer lives in the sibling oggopus package.
package opus
