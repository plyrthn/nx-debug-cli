package remoteaudio

import (
	"fmt"
	"sync"
	"time"

	"github.com/hajimehoshi/ebiten/v2/audio"
)

// buffered is how much converted audio is held back before playing. Enough to
// ride out a network hiccup, short enough that the sound still lines up with
// the picture.
const buffered = 200 * time.Millisecond

var (
	ctxOnce sync.Once
	ctxRate int
	ctx     *audio.Context
)

// sharedContext returns the process-wide audio context. Ebiten allows exactly
// one and fixes its sample rate at creation, so the first stream to open
// decides it; a later stream at a different rate is refused rather than played
// back at the wrong pitch.
func sharedContext(sampleRate int) (*audio.Context, error) {
	ctxOnce.Do(func() {
		ctxRate = sampleRate
		ctx = audio.NewContext(sampleRate)
	})
	if ctxRate != sampleRate {
		return nil, fmt.Errorf("remoteaudio: already playing at %dHz, cannot also play %dHz", ctxRate, sampleRate)
	}
	return ctx, nil
}

// Player takes chunks off the target's PCM stream and plays them.
type Player struct {
	format Format
	ring   *ring
	player *audio.Player
}

func NewPlayer(f Format) (*Player, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	actx, err := sharedContext(f.SampleRate)
	if err != nil {
		return nil, err
	}

	r := newRing(int(time.Duration(f.SampleRate) * buffered / time.Second * frameBytes))
	p, err := actx.NewPlayer(r)
	if err != nil {
		return nil, err
	}
	p.SetBufferSize(buffered)
	p.Play()
	return &Player{format: f, ring: r, player: p}, nil
}

// Write queues one chunk exactly as it came off the stream. It never blocks:
// if playback has fallen behind, the oldest audio is dropped instead, since
// the point of a live view is to be current rather than complete.
func (p *Player) Write(pcm []byte) {
	p.ring.Write(p.format.ToStereo16(pcm))
}

// Stats reports dropped and silence-filled bytes, both measured in converted
// output bytes. See ring.stats.
func (p *Player) Stats() (dropped, starved int64) { return p.ring.stats() }

func (p *Player) Close() error { return p.player.Close() }
