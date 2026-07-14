#!/usr/bin/env python3
"""Report the Opus mode distribution of an Ogg Opus file, measured from the bitstream.

Every Opus packet's first byte is the TOC (table of contents). Its top five bits
are the "config" number, and RFC 6716 section 3.1 maps config to the coding mode:

    config  0..11  ->  SILK-only
    config 12..15  ->  hybrid (SILK + CELT)
    config 16..31  ->  CELT-only

go-opus's encoder is CELT-only. libopus in its default mode picks a mode per
frame, so a benchmark that does not check this is not comparing like with like.
This script reads the modes the encoder actually emitted rather than trusting
whatever flag we passed it.

usage: opus-modes.py [--summary] file.opus
"""

import sys

# RFC 6716 section 3.1 config -> mode boundaries.
SILK_MAX = 11
HYBRID_MAX = 15

# RFC 6716 section 3.1: frame size in ms, indexed by config number.
FRAME_MS = (
    # config 0..11, SILK-only: 10, 20, 40, 60 ms per bandwidth
    [10, 20, 40, 60] * 3
    # config 12..15, hybrid: 10, 20 ms per bandwidth
    + [10, 20] * 2
    # config 16..31, CELT-only: 2.5, 5, 10, 20 ms per bandwidth
    + [2.5, 5, 10, 20] * 4
)


def mode_of(config):
    """Map a TOC config number to its coding mode."""
    if config <= SILK_MAX:
        return "SILK"
    if config <= HYBRID_MAX:
        return "hybrid"
    return "CELT"


def ogg_packets(f):
    """Yield each logical Ogg packet, reassembling packets that span pages."""
    buf = bytearray()
    while True:
        cap = f.read(4)
        if not cap:
            return
        if cap != b"OggS":
            raise SystemExit(f"not an Ogg stream: bad capture pattern {cap!r}")
        # version(1) type(1) granule(8) serial(4) seq(4) crc(4) nsegs(1) = 23 bytes
        head = f.read(23)
        if len(head) < 23:
            raise SystemExit("truncated Ogg page header")
        nsegs = head[22]
        segs = f.read(nsegs)
        if len(segs) < nsegs:
            raise SystemExit("truncated Ogg segment table")
        body = f.read(sum(segs))

        off = 0
        for seg in segs:
            buf += body[off : off + seg]
            off += seg
            # A lacing value below 255 terminates the packet; 255 means it
            # continues into the next segment (and possibly the next page).
            if seg < 255:
                yield bytes(buf)
                buf.clear()


def analyse(path):
    """Return (mode counts, config counts, total audio packets) for an Ogg Opus file."""
    modes = {}
    configs = {}
    total = 0
    with open(path, "rb") as f:
        for pkt in ogg_packets(f):
            if not pkt:
                continue
            # The two header packets are not audio and carry no TOC.
            if pkt.startswith(b"OpusHead") or pkt.startswith(b"OpusTags"):
                continue
            config = pkt[0] >> 3
            modes[mode_of(config)] = modes.get(mode_of(config), 0) + 1
            configs[config] = configs.get(config, 0) + 1
            total += 1
    return modes, configs, total


def main():
    args = [a for a in sys.argv[1:] if a != "--summary"]
    summary = "--summary" in sys.argv[1:]
    if len(args) != 1:
        raise SystemExit(f"usage: {sys.argv[0]} [--summary] file.opus")

    modes, configs, total = analyse(args[0])
    if total == 0:
        raise SystemExit(f"{args[0]}: no audio packets found")

    # Order the modes so the dominant one reads first.
    ranked = sorted(modes.items(), key=lambda kv: -kv[1])

    if summary:
        # One line, for a table cell: "CELT 100.0%"
        print(" + ".join(f"{m} {100.0 * n / total:.1f}%" for m, n in ranked))
        return

    print(f"{args[0]}: {total} audio packets")
    for mode, n in ranked:
        print(f"  {mode:<7} {n:>7} packets  {100.0 * n / total:6.2f}%")
    print("  by TOC config:")
    for config in sorted(configs):
        ms = FRAME_MS[config]
        print(
            f"    config {config:>2} ({mode_of(config):<6} {ms:>4} ms): "
            f"{configs[config]:>7} packets  {100.0 * configs[config] / total:6.2f}%"
        )


if __name__ == "__main__":
    main()
