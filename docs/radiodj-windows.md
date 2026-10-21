# RadioDJ on Windows as the music source (step 3)

The festival controller this year is a Windows laptop running RadioDJ.
RadioDJ plays into a virtual audio device; the Civitone controller captures
that device and feeds it to the Snapcast server as a PCM stream.

```
RadioDJ  ->  VB-CABLE "CABLE Input"  ->  civitone.exe (WASAPI capture)
         ->  snapserver.exe tcp://127.0.0.1:4953  ->  nodes
```

## Setup

1. Install VB-CABLE (free, vb-audio.com). Reboot.
2. RadioDJ: Options > Sound cards > set the main player output to
   "CABLE Input (VB-Audio Virtual Cable)". Keep the cue/PFL output on the
   laptop speakers or headphones.
3. Windows Sound settings: set "CABLE Output" sample rate to 48000 Hz,
   16 bit, so no resampling happens in the chain.
4. Put `snapserver.exe` (Snapcast GitHub release, Windows zip) next to
   `civitone.exe`.
5. Run `civitone.exe --audio-device "CABLE Output"`. The controller starts
   snapserver, captures the cable, and serves the console on :8080.

## Open question: capture library

Two options for WASAPI capture in Go:

- `github.com/gen2brain/malgo` (miniaudio). Cross-platform, well used, but
  cgo. Means building the Windows binary on the Windows laptop, or installing
  mingw on the Mac. Simple to start with.
- `github.com/moutend/go-wca` pure Go WASAPI via COM. No cgo, cross-compiles
  from the Mac, Windows only. More code.

Decide when step 3 starts. `internal/audio` is an interface so either fits.

## Latency note

The virtual cable adds roughly 10 to 20 ms. WASAPI shared mode adds a 10 ms
period. Both are inside the Parade budget. The announcer mic for the parade
plugs into the laptop's USB interface and is a second capture device.
