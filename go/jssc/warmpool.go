package jssc

import (
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/wizhut/jssc-go-oss/go/codec"
)

// minWarmGroups is the point below which spreading the inflate costs more than
// it saves.
const minWarmGroups = 8

// WarmPool inflates every pool group that is not already cached.
//
// Groups are independent by construction — the same guarantee the encoder
// relies on to compress them in parallel — so a reader that knows it will touch
// every string can inflate them all at once instead of one at a time on the
// path of a walk. A full decode knows exactly that.
func (a *Archive) WarmPool() error {
	p := a.vals
	if p.count == 0 {
		return nil
	}
	// Warming is the one caller that knows it wants every string, so every
	// segment is walked here rather than on the first miss inside it.
	if err := p.prepare(); err != nil {
		return err
	}
	type job struct {
		s *poolSegment
		g int
	}
	todo := make([]job, 0, p.groups)
	for k := range p.segs {
		s, err := p.segment(k)
		if err != nil {
			return err
		}
		first := k * segmentGroups
		for g := first; g < min(first+segmentGroups, p.groups); g++ {
			if !s.cached(g) {
				todo = append(todo, job{s, g})
			}
		}
	}
	if len(todo) == 0 {
		return nil
	}
	if len(todo) < minWarmGroups {
		for _, t := range todo {
			if err := a.decodeGroup(p, t.s, t.g); err != nil {
				return err
			}
		}
		return nil
	}

	workers := min(runtime.GOMAXPROCS(0), len(todo))
	plains := make([][]byte, len(todo))
	var (
		next     atomic.Int64
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			app, _ := a.codec.(codec.Appender)
			for {
				k := int(next.Add(1) - 1)
				if k >= len(todo) {
					return
				}
				// inflateGroup branches on the stored-raw flag, the same rule
				// decodeGroup follows, and it has to hold here too: running
				// the codec over uncompressed bytes would not fail, it would
				// quietly return something else.
				plain, err := inflateGroup(a.codec, app, p, todo[k].s, todo[k].g)
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = errCorruptf("decompress %s group %d: %v", p.name, todo[k].g, err)
					}
					mu.Unlock()
					return
				}
				plains[k] = plain
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}

	for k, t := range todo {
		lo, hi, err := p.fill(t.s, t.g, plains[k])
		if err != nil {
			return err
		}
		a.DecodedStrings += hi - lo
		a.DecodedGroups++
	}
	a.keyIndexStale = true
	return nil
}
