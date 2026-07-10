#!/usr/bin/env python3
"""TOC census of Opus test vector .bit files (opus_demo framing).

Usage: vector-census.py 'path/to/*.bit'
Produced the tables in docs/test-vectors.md; reusable as a sanity check in the
phase 0 vector fetch script.
"""
import struct, sys, glob, collections

BW = {**{c: "NB" for c in range(0, 4)}, **{c: "MB" for c in range(4, 8)},
      **{c: "WB" for c in range(8, 12)}, **{c: "SWB" for c in (12, 13)},
      **{c: "FB" for c in (14, 15)}, **{c: "NB" for c in range(16, 20)},
      **{c: "WB" for c in range(20, 24)}, **{c: "SWB" for c in range(24, 28)},
      **{c: "FB" for c in range(28, 32)}}

def mode(c):
    return "SILK" if c < 12 else ("HYBRID" if c < 16 else "CELT")

SILK_FS = {0: 10, 1: 20, 2: 40, 3: 60}
HYB_FS = {0: 10, 1: 20}
CELT_FS = {0: 2.5, 1: 5, 2: 10, 3: 20}

def fsize(c):
    if c < 12:
        return SILK_FS[c % 4]
    if c < 16:
        return HYB_FS[c % 2]
    return CELT_FS[c % 4]

for f in sorted(glob.glob(sys.argv[1])):
    data = open(f, 'rb').read()
    off = 0
    modes = collections.Counter(); bands = collections.Counter()
    fsz = collections.Counter(); codes = collections.Counter()
    stereo = collections.Counter()
    trans = 0; prev = None; n = 0
    while off + 8 <= len(data):
        ln, rng = struct.unpack('>II', data[off:off+8]); off += 8
        if ln == 0 or off + ln > len(data):
            off += ln
            continue
        toc = data[off]; off += ln
        cfg = toc >> 3; n += 1
        m = mode(cfg)
        modes[m] += 1; bands[BW[cfg]] += 1; fsz[fsize(cfg)] += 1
        codes[toc & 3] += 1; stereo[(toc >> 2) & 1] += 1
        if prev is not None and prev != m:
            trans += 1
        prev = m
    name = f.split('/')[-1]
    print(f"{name}: pkts={n} modes={dict(modes)} transitions={trans} "
          f"bw={dict(bands)} fs_ms={dict(sorted(fsz.items()))} "
          f"stereo={dict(stereo)} codes={dict(codes)}")
