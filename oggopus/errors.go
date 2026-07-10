package oggopus

import "errors"

// Sentinel errors returned by the oggopus package, testable with errors.Is.
var (
	// ErrInvalidConfig reports that a Config is not usable: an unsupported sample
	// rate or channel count, or an out-of-range bitrate or complexity.
	ErrInvalidConfig = errors.New("oggopus: invalid config")
	// ErrClosed reports an operation on an Encoder or Decoder after Close.
	ErrClosed = errors.New("oggopus: use of closed stream")
)

// errCodecNotWired is the seam sentinel. Until the phase-4 opus encoder and the
// phase-2/3 decoder are wired to the container (see codec.go), the PCM entry
// points (Encoder.Write/Close/EncodeInterleaved, Decoder.Read/WriteTo) return
// it. The container writer and reader in this package are complete and tested
// without a codec; only the PCM<->packet conversion depends on it.
//
// It is unexported because it is a temporary construction-phase signal, not a
// stable part of the API: once the codec lands these paths return real results
// and this sentinel disappears. In-package tests assert against it directly.
var errCodecNotWired = errors.New("oggopus: opus codec not wired yet (container-only build)")
