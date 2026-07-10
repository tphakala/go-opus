// Package rangecoding is a transliteration of the libopus range coder
// (celt/entenc.c, entdec.c, entcode.c). The coder is a value-copyable struct
// over a shared []byte so snapshot is struct assignment and byte splicing is
// copy(), which the encoder's two RDO features depend on (docs/hard-parts.md 1).
// This package is deliberately C-shaped and diffable against libopus v1.6.1.
package rangecoding
