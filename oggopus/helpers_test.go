package oggopus

// goldenHeadPage returns the byte-exact OpusHead BOS page used by the CRC and
// page golden tests. Its bytes (including the CRC field) were produced by an
// independent Python implementation of the Ogg framing and CRC, so a match
// proves the Go page writer agrees with a second implementation.
//
// Contents: OpusHead page, flags BOS, granule 0, serial 0x12345678, sequence 0,
// one 19-byte packet (mono, pre-skip 312, input rate 48000, gain 0, family 0).
func goldenHeadPage() []byte {
	return []byte{
		// Page header.
		0x4f, 0x67, 0x67, 0x53, // "OggS"
		0x00,                                           // version
		0x02,                                           // flags: BOS
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // granule 0
		0x78, 0x56, 0x34, 0x12, // serial 0x12345678
		0x00, 0x00, 0x00, 0x00, // sequence 0
		0x3d, 0x11, 0xc0, 0xbb, // CRC 0xbbc0113d (little-endian)
		0x01, // segment count
		0x13, // one segment, length 19
		// OpusHead packet body.
		0x4f, 0x70, 0x75, 0x73, 0x48, 0x65, 0x61, 0x64, // "OpusHead"
		0x01,       // version 1
		0x01,       // channels 1
		0x38, 0x01, // pre-skip 312
		0x80, 0xbb, 0x00, 0x00, // input sample rate 48000
		0x00, 0x00, // output gain 0
		0x00, // mapping family 0
	}
}

// testHead is a canonical mono OpusHead for tests.
func testHead(channels int, preSkip uint16) opusHead {
	return opusHead{
		version:         opusHeadVersion,
		channels:        byte(channels),
		preSkip:         preSkip,
		inputSampleRate: 48000,
		outputGain:      0,
		mappingFamily:   mappingFamily0,
	}
}
