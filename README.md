# go-macos/avfoundation

Hardware-accelerated video decoding on macOS from pure Go — `CGO_ENABLED=0`, via
[purego](https://github.com/ebitengine/purego).

```go
r, err := avfoundation.Open("movie.mp4")
defer r.Close()

for {
        f, err := r.NextFrame()
        if errors.Is(err, io.EOF) { break }
        // f.Pix aliases the decoder's buffer: Stride bytes per row, BGRA
        use(f)
        f.Release()
}
```

## Why this exists

There is no realistic pure-Go alternative. A software H.264 or HEVC decoder
written in Go will not keep up with 4K60; the hardware decoder in every Mac does
it without warming up. Measured here on an M4 Max, 720p H.264:

```
decoded 7740 frames in 3.575s  (2165 fps, 72x real time)
```

The constraint the fleet actually cares about is **no cgo**, not "no operating
system". This is a system framework reached without a C toolchain.

## Design notes

**Frames are not copied.** A `Frame` holds the decoder's buffer locked and its
pixels stay valid until `Release()`. At 4K that avoids ~33 MB of copying per
frame. Every frame you receive must be released, once — holding many unreleased
frames stalls the decoder, which is waiting for its own buffers back.

**Use `Stride`, not `Width*4`.** Decoders pad rows: a 1280-wide frame comes back
with a 5120-byte stride here, but that is not guaranteed. Indexing by width
shears the picture progressively down the frame, which looks like a decode bug
and is not one.

**The caller owns the clock.** This is a pull API: frames come out as fast as
they decode, each carrying its presentation timestamp. That is the right shape
for a renderer with its own frame loop — an immersive viewer must draw when the
display is ready, not when a player decides.

## Two measured limits

**BGRA only.** Asking for RGBA does not convert — it makes the reader *fail*
(`AVAssetReaderStatus` 3), and the failure surfaced as an opaque status code from
deep inside AVFoundation. `Open` therefore refuses a non-decodable format up
front with a reason, and `Frame.ToRGBA` converts. The planar YUV formats the
hardware natively prefers (NV12 and friends) need a multi-plane `Frame` this
package does not have yet; that is the path to a zero-conversion Metal pipeline.

**No Matroska.** AVFoundation does not demux MKV or WebM: `Open` reports
`ErrNoVideoTrack` for both. MP4, MOV and M4V work. That path exists elsewhere:
demux with [`go-avkit/avkit/container`](https://github.com/go-avkit/avkit),
which does read EBML, and decode with
[`go-macos/videotoolbox`](https://github.com/go-macos/videotoolbox), which
feeds the elementary stream to VideoToolbox directly and hands back the same
zero-copy BGRA `Frame` this package does. Its `cmd/vtprobe` decodes a 1 h 32 MKV
end to end, and its first ten frames of a control MP4 are byte-for-byte what
`avprobe` produces here.

## `cmd/avprobe`

```
go run ./cmd/avprobe movie.mp4              # info + first 3 frames as PNG
go run ./cmd/avprobe -n 10 movie.mp4
go run ./cmd/avprobe -all movie.mp4         # decode everything, report the rate
```

## Testing

The portable layer is at **100% statement coverage** behind platform seams. The
purego bindings need real media, which a CI runner has no business shipping, so
they are covered two ways: error paths (absent, junk and empty files, refused
formats) run everywhere, and a live decode test runs when
`AVFOUNDATION_TEST_FILE` names a video file:

```
AVFOUNDATION_TEST_FILE=/path/to/movie.mp4 go test -race ./...
```

That lane asserts only things checkable from outside the package — the
dimensions and frame rate the file itself reports, and that presentation
timestamps advance by about one frame period. With it on, the bindings reach
~86%; the rest is failure branches that need a broken decoder rather than a
broken file.

Licence: BSD-3-Clause.
