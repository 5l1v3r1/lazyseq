package lazyrnn

import (
	"sync"

	"github.com/unixpickle/anydiff"
	"github.com/unixpickle/anydiff/anyseq"
	"github.com/unixpickle/anyvec"
	"github.com/unixpickle/essentials"
)

type packRes struct {
	C   anyvec.Creator
	Ins []Seq
	Out <-chan *anyseq.Batch

	Done        <-chan struct{}
	LanesPerSeq []int
	Lens        []int
	V           anydiff.VarSet
}

// Pack aggregates multiple Seqs together into a single
// Seq with larger batches.
func Pack(c anyvec.Creator, seqs []Seq) Seq {
	outChan := make(chan *anyseq.Batch, 1)
	doneChan := make(chan struct{})

	res := &packRes{
		C:           c,
		Ins:         seqs,
		Out:         outChan,
		Done:        doneChan,
		LanesPerSeq: make([]int, len(seqs)),
		Lens:        make([]int, len(seqs)),
		V:           anydiff.VarSet{},
	}

	go res.forward(outChan, doneChan)

	return res
}

func (p *packRes) Creator() anyvec.Creator {
	return p.C
}

func (p *packRes) Forward() <-chan *anyseq.Batch {
	return p.Out
}

func (p *packRes) Vars() anydiff.VarSet {
	<-p.Done
	return p.V
}

func (p *packRes) Propagate(upstream <-chan *anyseq.Batch, grad *Grad) {
	for _ = range p.Forward() {
	}

	downstreams := make([]chan<- *anyseq.Batch, len(p.Ins))
	var wg sync.WaitGroup
	for i, in := range p.Ins {
		var needProp bool
		grad.Use(func(g anydiff.Grad) {
			needProp = g.Intersects(in.Vars())
		})
		if !needProp {
			continue
		}
		wg.Add(1)
		ch := make(chan *anyseq.Batch, 1)
		go func(in Seq, ch <-chan *anyseq.Batch) {
			in.Propagate(ch, grad)
			wg.Done()
		}(in, ch)
		downstreams[i] = ch
	}

	for upBatch := range upstream {
		for i, part := range p.splitUpstream(upBatch) {
			if part != nil && downstreams[i] != nil {
				downstreams[i] <- part
			}
		}
	}

	for _, ch := range downstreams {
		if ch != nil {
			close(ch)
		}
	}

	wg.Wait()
}

func (p *packRes) forward(out chan<- *anyseq.Batch, done chan<- struct{}) {
	c := p.Creator()

	for {
		var numOpen int
		var batches []*anyseq.Batch
		for inIdx, in := range p.Ins {
			batch, ok := <-in.Forward()
			if ok {
				numOpen++
				batches = append(batches, batch)
				p.LanesPerSeq[inIdx] = len(batch.Present)
				p.Lens[inIdx]++
			} else {
				lanes := p.LanesPerSeq[inIdx]
				batches = append(batches, fillerBatch(c, lanes))
			}
		}
		if numOpen == 0 {
			break
		}
		out <- joinBatches(c, batches)
	}

	for _, in := range p.Ins {
		p.V = anydiff.MergeVarSets(p.V, in.Vars())
	}

	close(out)
	close(done)
}

type packRereaderRes struct {
	*packRes
	Rereaders []Rereader
}

// PackRereader is like Pack, but for Rereaders.
func PackRereader(c anyvec.Creator, rs []Rereader) Rereader {
	plain := make([]Seq, len(rs))
	for i, x := range rs {
		plain[i] = x
	}
	return &packRereaderRes{
		packRes:   Pack(c, plain).(*packRes),
		Rereaders: rs,
	}
}

func (p *packRereaderRes) Reread(start, end int) <-chan *anyseq.Batch {
	<-p.packRes.Done

	if start > end || start < 0 {
		panic("slice bounds out of range")
	}

	sourceChans := make([]<-chan *anyseq.Batch, len(p.Ins))
	var maxLen int
	for i, seqLen := range p.Lens {
		maxLen = essentials.MaxInt(maxLen, seqLen)
		if seqLen <= start {
			empty := make(chan *anyseq.Batch)
			sourceChans[i] = empty
			close(empty)
		} else {
			subEnd := end
			if subEnd > seqLen {
				subEnd = seqLen
			}
			sourceChans[i] = p.Rereaders[i].Reread(start, subEnd)
		}
	}

	if end > maxLen {
		panic("slice bounds out of range")
	}

	out := make(chan *anyseq.Batch, 1)

	go func() {
		c := p.Creator()
		for i := start; i < end; i++ {
			var batches []*anyseq.Batch
			for inIdx, ch := range sourceChans {
				batch, ok := <-ch
				if ok {
					batches = append(batches, batch)
					p.LanesPerSeq[inIdx] = len(batch.Present)
					p.Lens[inIdx]++
				} else {
					lanes := p.LanesPerSeq[inIdx]
					batches = append(batches, fillerBatch(c, lanes))
				}
			}
			out <- joinBatches(c, batches)
		}
		close(out)
	}()

	return out
}

// splitUpstream splits an upstream batch into upstream
// batches for each input.
// If an input is not present yet, its batch is nil.
func (p *packRes) splitUpstream(upBatch *anyseq.Batch) []*anyseq.Batch {
	vecSize := upBatch.Packed.Len() / upBatch.NumPresent()
	res := make([]*anyseq.Batch, len(p.Ins))

	var laneOffset int
	var vecOffset int
	for inIdx, numLanes := range p.LanesPerSeq {
		subBatch := &anyseq.Batch{
			Present: upBatch.Present[laneOffset : laneOffset+numLanes],
		}
		if subBatch.NumPresent() > 0 {
			subBatch.Packed = upBatch.Packed.Slice(vecOffset*vecSize,
				(vecOffset+subBatch.NumPresent())*vecSize)
			res[inIdx] = subBatch
			vecOffset += subBatch.NumPresent()
		}
		laneOffset += numLanes
	}

	return res
}

func joinBatches(c anyvec.Creator, batches []*anyseq.Batch) *anyseq.Batch {
	var packed []anyvec.Vector
	var present []bool
	for _, batch := range batches {
		// NumPresent is 0 if this is a filler batch.
		if batch.NumPresent() != 0 {
			packed = append(packed, batch.Packed)
		}
		present = append(present, batch.Present...)
	}
	return &anyseq.Batch{
		Packed:  c.Concat(packed...),
		Present: present,
	}
}

// fillerBatch creates a placeholder batch that signifies
// that a sequence batch has ended.
func fillerBatch(c anyvec.Creator, lanes int) *anyseq.Batch {
	return &anyseq.Batch{Present: make([]bool, lanes)}
}
