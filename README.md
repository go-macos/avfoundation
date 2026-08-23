# go-macos/avfoundation

Hardware-accelerated video decoding **and playback** on macOS from pure Go —
`CGO_ENABLED=0`, via [purego](https://github.com/ebitengine/purego).

There are two ways in, and they are not interchangeable.

**`Reader` — decode as fast as the hardware allows.** No clock, no sound.

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

**`Player` — real-time playback.** Sound, pause, seek, speed and volume.

```go
p, err := avfoundation.OpenPlayer("movie.mp4")
defer p.Close()

p.SetVolume(0.8)
p.Seek(90 * time.Second)
p.Play()

for drawing {
        p.Pump(4 * time.Millisecond) // only where nothing else runs the run loop
        f, err := p.TryFrame()
        if f == nil { continue }     // nothing new: draw the last picture again
        use(f)
        f.Release()
}
```

### Which one

| | `Reader` | `Player` |
|---|---|---|
| Audio | no | **yes** |
| Pause, seek, rate, volume | no | **yes** |
| Owns the clock | you do | AVPlayer does |
| Frames | every one, in order | the one belonging to *now*, dropped to stay in sync |
| Speed | 2165 fps measured (72× real time) | real time, by definition |
| Ends with | `io.EOF` | never — it is a clock, ask it where it is |

Use `Reader` to transcode, analyse, or render on your own schedule. Use
`Player` when a person is watching and listening.

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

**With `Reader`, the caller owns the clock.** It is a pull API: frames come out
as fast as they decode, each carrying its presentation timestamp. That is the
right shape for a renderer with its own frame loop — an immersive viewer must
draw when the display is ready, not when a player decides. `Player` is the other
bargain: AVPlayer keeps the clock, and gives you audio for it.

## Two measured limits

**BGRA only**, for both paths. Asking for RGBA does not convert — it makes the reader *fail*
(`AVAssetReaderStatus` 3), and the failure surfaced as an opaque status code from
deep inside AVFoundation. `Open` therefore refuses a non-decodable format up
front with a reason, and `Frame.ToRGBA` converts. The planar YUV formats the
hardware natively prefers (NV12 and friends) need a multi-plane `Frame` this
package does not have yet; that is the path to a zero-conversion Metal pipeline.

**No Matroska.** AVFoundation does not demux MKV or WebM: `Open` and
`OpenPlayer` both report `ErrNoVideoTrack` for them. MP4, MOV and M4V work. For Matroska, demux with
`go-avkit/avkit/container` (which does read EBML, and can recover a track's
parameter sets from its samples) and feed the elementary stream to VideoToolbox
directly — a separate job from this package.

## Player notes

**`TryFrame`, not `NextFrame`.** `AVPlayerItemVideoOutput` answers about a
*moment in time*, not about a position in a stream: it vends at most one buffer
per item time, and after a seek or a rate change the moment can go backwards.
`TryFrame` returns `(nil, nil)` when there is nothing new, which is the common
case — a display loop polls faster than the video's frame rate. Draw the last
picture again.

**Seeks are exact.** `Seek` goes to AVFoundation with zero tolerance, so it lands
on the time asked for rather than on the nearest keyframe: measured, `Seek(60s)`
lands at 60.000000s, where a plain `seekToTime:` lands at 58.333s. That costs
decoding forward from the previous keyframe, which is the trade a scrubber wants.

**Open it on the main thread — this one is not negotiable.** AVFoundation loads
a file through the *main dispatch queue*, and only the main thread's run loop
drains that queue. Measured: with the main thread parked, an `AVPlayerItem` sits
at status 0 forever, however hard another thread's run loop is pumped, and on a
cold file reports a duration of 0 with it. Start the main run loop and the same
item loads in ~100 ms.

The nasty part is that a player in that state still *answers*: seek it to 90s and
it reports 90s — the value you just handed it. That echo is how a binding that has
opened nothing looks exactly like one that works. `OpenPlayer` therefore runs the
run loop itself while loading and returns an *error* rather than a half-loaded
`Player`, and every live test here asserts something only a loaded asset could
know. `TestLiveLoadNeedsTheMainRunLoop` is the control: it switches the main run
loop off, shows the echo, switches it back on and shows the item load.

So: `runtime.LockOSThread()` on the main goroutine, open the player there, drive
it from there. A GUI application already does this — the main run loop *is* the
event loop, and nothing extra is needed. A program with no event loop must run
one; that is what `Pump` is for.

*After* loading, measured again, the clock advances and frames come out with
nothing but `time.Sleep` between the calls. `Pump` is what a headless loop should
wait with rather than sleeping; it is not the engine. A `Player` is not safe for
concurrent use.

## `cmd/avprobe` and `cmd/avplay`

```
go run ./cmd/avprobe movie.mp4              # info + first 3 frames as PNG
go run ./cmd/avprobe -n 10 movie.mp4
go run ./cmd/avprobe -all movie.mp4         # decode everything, report the rate

go run ./cmd/avplay -for 5s movie.mp4                    # play it, with sound
go run ./cmd/avplay -for 3s -seek 2m -rate 2 movie.mp4   # from 2m, double speed
go run ./cmd/avplay -for 2s -rate -1 -png f.png movie.mp4 # backwards, one frame out
```

Measured on an M4 Max, 720p H.264:

```
  playing at rate 1, volume 0.2 for 4s
  118 frames vended over 4.004s wall (29.5 fps), 759 polls had nothing new
  clock moved 3.922s (0.98x wall), frame PTS 0s -> 3.902s

  playing at rate 2, volume 0 for 3s
  176 frames vended over 3.002s wall (58.6 fps), 487 polls had nothing new
  clock moved 5.842s (1.95x wall), frame PTS 2m0s -> 2m5.841s
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
dimensions and frame rate the file itself reports, that presentation timestamps
advance by about one frame period, that the player's clock advances by itself and
that a seek lands where it was sent. It also cross-checks the two paths against
each other: `Reader` and `Player` must agree about the file's duration and size.

The trap it is built around is the one described above. Every live assertion is
something only a *loaded* asset can answer — a status, a duration, a clock that
moves — never merely "the value I passed came back", which an item that opened
nothing answers just as readily.

Licence: BSD-3-Clause.
