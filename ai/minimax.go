package ai

import (
	"bytes"
	"log"
	"math/rand"
	"sync/atomic"
	"time"

	"golang.org/x/net/context"

	"github.com/nelhage/taktician/bitboard"
	"github.com/nelhage/taktician/ptn"
	"github.com/nelhage/taktician/tak"
)

const (
	MaxEval      int64 = 1 << 30
	MinEval            = -MaxEval
	WinThreshold       = 1 << 29

	tableSize uint64 = (1 << 20)

	maxDepth = 15
	maxMoves = 500

	multiCutSearch    = 6
	multiCutThreshold = 3
)

type EvaluationFunc func(c *bitboard.Constants, p *tak.Position) int64

type MinimaxAI struct {
	cfg  MinimaxConfig
	rand *rand.Rand

	st Stats
	c  bitboard.Constants

	history  map[uint64]int
	response map[uint64]tak.Move

	evaluate EvaluationFunc

	table []tableEntry
	depth int
	stack [maxDepth]struct {
		p  *tak.Position
		mg moveGenerator
		pv [maxDepth]tak.Move
		m  tak.Move

		moves [maxMoves]tak.Move
		vals  [maxMoves]int
	}

	cancel *int32
}

type tableEntry struct {
	hash  uint64
	depth int
	value int64
	bound boundType
	m     tak.Move
}

type boundType byte

const (
	lowerBound = iota
	exactBound = iota
	upperBound = iota
)

type Stats struct {
	Depth    int
	Canceled bool
	Elapsed  time.Duration

	Generated uint64
	Evaluated uint64
	Scout     uint64
	Terminal  uint64
	Visited   uint64

	CutNodes   uint64
	NullSearch uint64
	NullCut    uint64
	Cut0       uint64
	Cut1       uint64
	CutSearch  uint64

	ReSearch uint64

	AllNodes uint64

	TTHits     uint64
	TTShortcut uint64

	Extensions    uint64
	ReducedSlides uint64

	MCSearch uint64
	MCCut    uint64
}

func (s Stats) Merge(other Stats) Stats {
	s.Generated += other.Generated
	s.Evaluated += other.Evaluated
	s.Scout += other.Scout
	s.Terminal += other.Terminal
	s.Visited += other.Visited
	s.CutNodes += other.CutNodes
	s.NullSearch += other.NullSearch
	s.NullCut += other.NullCut
	s.Cut0 += other.Cut0
	s.Cut1 += other.Cut1
	s.CutSearch += other.CutSearch
	s.ReSearch += other.ReSearch
	s.AllNodes += other.AllNodes
	s.TTHits += other.TTHits
	s.TTShortcut += other.TTShortcut
	s.Extensions += other.Extensions
	s.ReducedSlides += other.ReducedSlides
	s.MCSearch += other.MCSearch
	s.MCCut += other.MCCut
	return s
}

type MinimaxConfig struct {
	Size  int
	Depth int
	Debug int
	Seed  int64

	RandomizeWindow int64
	RandomizeScale  int64

	NoSort         bool
	NoTable        bool
	NoNullMove     bool
	NoExtendForces bool

	NoReduceSlides bool
	NoMultiCut     bool

	Evaluate EvaluationFunc
}

// MakePrecise modifies a MinimaxConfig to produce a MinimaxAI that
// will always produce accurate game-theoretic evaluations – i.e. it
// disables all heuristic searches that cannot prove the correctness
// of their results.
//
// In general, such configurations should be slower and weaker
// players, but can be useful for constructing or solving puzzles,
// debugging, or analyzing unusual positions.
func (cfg *MinimaxConfig) MakePrecise() {
	cfg.NoNullMove = true
	cfg.NoExtendForces = true
	cfg.NoReduceSlides = true
	cfg.NoMultiCut = true
}

func NewMinimax(cfg MinimaxConfig) *MinimaxAI {
	m := &MinimaxAI{cfg: cfg}
	if m.cfg.Depth == 0 {
		m.cfg.Depth = maxDepth
	}
	if m.cfg.RandomizeScale == 0 {
		m.cfg.RandomizeScale = 1
	}
	m.precompute()
	m.evaluate = cfg.Evaluate
	if m.evaluate == nil {
		m.evaluate = MakeEvaluator(cfg.Size, nil)
	}
	m.history = make(map[uint64]int, m.cfg.Size*m.cfg.Size*m.cfg.Size)
	m.response = make(map[uint64]tak.Move, m.cfg.Size*m.cfg.Size*m.cfg.Size)
	if !cfg.NoTable {
		m.table = make([]tableEntry, tableSize)
	}
	for i := range m.stack {
		m.stack[i].p = tak.Alloc(m.cfg.Size)
	}
	return m
}

const hashMul = 0x61C8864680B583EB

func (m *MinimaxAI) ttGet(h uint64) *tableEntry {
	if m.cfg.NoTable {
		return nil
	}
	i1 := h % tableSize
	i2 := (h * hashMul) % tableSize
	te := &m.table[i1]
	if te.hash == h {
		return te
	}
	te = &m.table[i2]
	if te.hash == h {
		return te
	}
	return nil
}

func (m *MinimaxAI) ttPut(h uint64) *tableEntry {
	if m.cfg.NoTable {
		return nil
	}
	if atomic.LoadInt32(m.cancel) != 0 {
		return nil
	}
	i1 := h % tableSize
	i2 := (h * hashMul) % tableSize
	if m.table[i1].hash != 0 {
		m.table[i2] = m.table[i1]
	}
	return &m.table[i1]
}

func (m *MinimaxAI) precompute() {
	s := uint(m.cfg.Size)
	m.c = bitboard.Precompute(s)
}

func formatpv(ms []tak.Move) string {
	var out bytes.Buffer
	out.WriteString("[")
	for i, m := range ms {
		if i != 0 {
			out.WriteString(" ")
		}
		out.WriteString(ptn.FormatMove(&m))
	}
	out.WriteString("]")
	return out.String()
}

func (ai *MinimaxAI) GetMove(ctx context.Context, p *tak.Position) tak.Move {
	pv, v, st := ai.Analyze(ctx, p)
	if ai.cfg.RandomizeWindow == 0 {
		return pv[0]
	}
	if v > WinThreshold || v < -WinThreshold {
		return pv[0]
	}
	rv := pv[0]
	base := v - ai.cfg.RandomizeWindow
	var i int64
	mg := &ai.stack[0].mg
	*mg = moveGenerator{
		ai:    ai,
		ply:   0,
		depth: st.Depth,
		p:     p,
		pv:    pv,
	}

	for m, child := mg.Next(); child != nil; m, child = mg.Next() {
		ai.stack[0].m = m
		_, cv := ai.pvSearch(child, 1, st.Depth-1, pv[1:],
			-v-1, -base)
		cv = -cv
		if cv <= base {
			continue
		}
		pts := (cv - base) / ai.cfg.RandomizeScale
		i += pts
		if ai.cfg.Debug > 2 {
			log.Printf("rand m=%s v=%d cv=%d pts=%d i=%d",
				ptn.FormatMove(&m), v, cv, pts, i)
		}
		if ai.rand.Int63n(i) <= pts {
			rv = m
		}
	}

	return rv
}

func (ai *MinimaxAI) AnalyzeAll(ctx context.Context, p *tak.Position) ([][]tak.Move, int64, Stats) {
	pv, v, st := ai.Analyze(ctx, p)
	mg := &ai.stack[0].mg
	*mg = moveGenerator{
		ai:    ai,
		ply:   0,
		depth: st.Depth,
		p:     p,
		pv:    pv,
	}
	if ai.cfg.Debug > 1 {
		log.Printf("[all-search] begin search depth=%d pv=%s v=%d",
			st.Depth, formatpv(pv), v)
	}
	out := [][]tak.Move{pv}
	for m, child := mg.Next(); child != nil; m, child = mg.Next() {
		ai.stack[0].m = m
		// we want to find moves in (v-1, v+1) (i.e. == v). We
		// invert and negate that to find the α-β window for
		// the child search: (-v-1, -v+1)
		ms, cv := ai.pvSearch(child, 1, st.Depth-1, pv[1:], -v-1, -v+1)
		cv = -cv
		if ai.cfg.Debug > 2 {
			log.Printf("[all-search] m=%s v=%d pv=%s",
				ptn.FormatMove(&m), cv, formatpv(ms))
		}
		if cv != v {
			continue
		}
		if m.Equal(&pv[0]) {
			continue
		}
		outpv := []tak.Move{m}
		out = append(out, append(outpv, ms...))
	}
	return out, v, st
}

func (m *MinimaxAI) Analyze(ctx context.Context, p *tak.Position) ([]tak.Move, int64, Stats) {
	if m.cfg.Size != p.Size() {
		panic("Analyze: wrong size")
	}
	for i, v := range m.history {
		m.history[i] = v / 2
	}
	var cancel int32
	m.cancel = &cancel
	go func() {
		<-ctx.Done()
		atomic.StoreInt32(&cancel, 1)
	}()

	var seed = m.cfg.Seed
	if seed == 0 {
		seed = time.Now().Unix()
	}
	m.rand = rand.New(rand.NewSource(seed))
	if m.cfg.Debug > 0 {
		log.Printf("start search ply=%d color=%s seed=%d",
			p.MoveNumber(), p.ToMove(), seed)
	}
	deadline, limited := ctx.Deadline()

	var next []tak.Move
	ms := make([]tak.Move, 0, maxDepth)
	var v, nv int64
	top := time.Now()
	var prevEval uint64
	var branchSum uint64
	base := 0
	te := m.ttGet(p.Hash())
	if te != nil && te.bound == exactBound {
		base = te.depth
		ms = append(ms[:0], te.m)
	}

	var st Stats
	for i := 1; i+base <= m.cfg.Depth; i++ {
		m.st = Stats{Depth: i + base}
		start := time.Now()
		m.depth = i + base
		next, nv = m.pvSearch(p, 0, i+base, ms, MinEval-1, MaxEval+1)
		if next == nil || atomic.LoadInt32(m.cancel) != 0 {
			st.Canceled = true
			break
		}
		v = nv
		st = m.st.Merge(st)
		ms = append(ms[:0], next...)
		timeUsed := time.Since(top)
		timeMove := time.Since(start)
		if m.cfg.Debug > 0 {
			log.Printf("[minimax] deepen: depth=%d val=%d pv=%s time=%s total=%s evaluated=%d tt=%d/%d branch=%d",
				base+i, v, formatpv(ms),
				timeMove,
				timeUsed,
				m.st.Evaluated,
				m.st.TTShortcut,
				m.st.TTHits,
				m.st.Evaluated/(prevEval+1),
			)
		}
		if m.cfg.Debug > 1 {
			log.Printf("[minimax]  stats: visited=%d m/ms=%f cut=%d all=%d cut0=%d(%2.2f) cut1=%d(%2.2f) m/cut=%2.2f",
				m.st.Visited,
				float64(m.st.Visited+m.st.Evaluated)/float64(timeMove.Seconds()*1000),
				m.st.CutNodes,
				m.st.AllNodes,
				m.st.Cut0,
				float64(m.st.Cut0)/float64(m.st.CutNodes+1),
				m.st.Cut1,
				float64(m.st.Cut0+m.st.Cut1)/float64(m.st.CutNodes+1),
				float64(m.st.CutSearch)/float64(m.st.CutNodes-m.st.Cut0-m.st.Cut1+1),
			)
			log.Printf("[minimax]         scout=%d null=%d/%d mc=%d/%d research=%d extend=%d rslide=%d",
				m.st.Scout,
				m.st.NullCut,
				m.st.NullSearch,
				m.st.MCCut,
				m.st.MCSearch,
				m.st.ReSearch,
				m.st.Extensions,
				m.st.ReducedSlides,
			)
		}
		if i > 1 {
			branchSum += m.st.Evaluated / (prevEval + 1)
		}
		prevEval = m.st.Evaluated
		if v > WinThreshold || v < -WinThreshold {
			break
		}
		if limited && i+base != m.cfg.Depth {
			var branch uint64
			if i > 2 {
				// conservatively multiply by 2 to
				// account for the bimodal branching
				// factor
				branch = 2 * branchSum / uint64(i-1)
			} else {
				// conservative estimate if we haven't
				// run enough plies to have one
				// yet. This can matter if the table
				// returns a deep move
				branch = 20
			}
			estimate := time.Now().Add(time.Since(start) * time.Duration(branch))
			if estimate.After(deadline) {
				if m.cfg.Debug > 0 {
					log.Printf("[minimax] time cutoff: depth=%d used=%s estimate=%s",
						base+i, timeUsed, estimate.Sub(top))
				}
				break
			}
		}
	}
	st.Elapsed = time.Since(top)
	return ms, v, st
}

func (m *MinimaxAI) Evaluate(p *tak.Position) int64 {
	return m.evaluate(&m.c, p)
}

func teSuffices(te *tableEntry, depth int, α, β int64) bool {
	if te.depth >= depth {
		switch {
		case te.bound == exactBound:
			return true
		case te.value < α && te.bound == upperBound:
			return true
		case te.value > β && te.bound == lowerBound:
			return true
		}
	}

	if te.bound == exactBound &&
		(te.value > WinThreshold || te.value < -WinThreshold) {
		return true
	}
	return false
}

func (ai *MinimaxAI) recordCut(m *tak.Move, move, depth, ply int) {
	ai.st.CutNodes++
	switch move {
	case 1:
		ai.st.Cut0++
	case 2:
		ai.st.Cut1++
	default:
		ai.st.CutSearch += uint64(move + 1)
	}
	ai.history[m.Hash()] += (1 << uint(depth))
	if ply > 0 {
		ai.response[ai.stack[ply-1].m.Hash()] = *m
	}
}

func (ai *MinimaxAI) pvSearch(
	p *tak.Position,
	ply, depth int,
	pv []tak.Move,
	α, β int64) ([]tak.Move, int64) {
	over, _ := p.GameOver()
	if depth <= 0 || over {
		ai.st.Evaluated++
		if over {
			ai.st.Terminal++
		}
		return nil, ai.evaluate(&ai.c, p)
	}

	ai.st.Visited++
	if β == α+1 {
		ai.st.Scout++
	}

	te := ai.ttGet(p.Hash())
	if te != nil {
		ai.st.TTHits++
		if teSuffices(te, depth, α, β) {
			_, e := p.MovePreallocated(&te.m, ai.stack[ply].p)
			if e == nil {
				ai.st.TTShortcut++
				ai.stack[ply].pv[0] = te.m
				return ai.stack[ply].pv[:1], te.value
			}
			te = nil
		}
	}

	// As of 1.6.2, Go's escape analysis can't tell that a
	// stack-allocated object here doesn't escape. So we force it
	// into our manual stack.
	mg := &ai.stack[ply].mg
	*mg = moveGenerator{
		ai:    ai,
		ply:   ply,
		depth: depth,
		p:     p,
		te:    te,
		pv:    pv,
	}

	best := ai.stack[ply].pv[:0]
	best = append(best, pv...)
	improved := false
	var i int
	for m, child := mg.Next(); child != nil; m, child = mg.Next() {
		i++
		var ms []tak.Move
		var newpv []tak.Move
		var v int64
		if len(best) != 0 {
			newpv = best[1:]
		}
		ai.stack[ply].m = m
		if i > 1 {
			ms, v = ai.zwSearch(child, ply+1, depth-1, newpv, -α-1, true)
			if -v > α && -v < β {
				ai.st.ReSearch++
				ms, v = ai.pvSearch(child, ply+1, depth-1, newpv, -β, -α)
			}
		} else {
			ms, v = ai.pvSearch(child, ply+1, depth-1, newpv, -β, -α)
		}
		v = -v
		if ai.cfg.Debug > 4+ply {
			log.Printf("%*s search ply=%d d=%d e=%d m=%s w=(%d,%d) v=%d pv=%s",
				ply, "", ply, depth, ai.st.Extensions,
				ptn.FormatMove(&m), α, β, v, formatpv(ms))
		}

		if len(best) == 0 {
			best = append(best[:0], m)
			best = append(best, ms...)
		}
		if v > α {
			improved = true
			best = append(best[:0], m)
			best = append(best, ms...)
			α = v
			if α >= β {
				ai.recordCut(&m, i, depth, ply)
				break
			}
		}
		if atomic.LoadInt32(ai.cancel) != 0 {
			return nil, 0
		}
	}

	hash := p.Hash()
	if te = ai.ttPut(hash); te != nil && (te.hash != hash || te.depth <= depth) {
		te.hash = hash
		te.depth = depth
		te.m = best[0]
		te.value = α
		if !improved {
			te.bound = upperBound
			ai.st.AllNodes++
		} else if α >= β {
			te.bound = lowerBound
		} else {
			te.bound = exactBound
		}
	}

	return best, α
}

func (ai *MinimaxAI) zwSearch(
	p *tak.Position,
	ply, depth int,
	pv []tak.Move,
	α int64, cut bool) ([]tak.Move, int64) {
	over, _ := p.GameOver()
	if depth <= 0 || over {
		ai.st.Evaluated++
		if over {
			ai.st.Terminal++
		}
		return nil, ai.evaluate(&ai.c, p)
	}

	ai.st.Visited++
	ai.st.Scout++

	te := ai.ttGet(p.Hash())
	if te != nil {
		ai.st.TTHits++
		if teSuffices(te, depth, α, α+1) {
			_, e := p.MovePreallocated(&te.m, ai.stack[ply].p)
			if e == nil {
				ai.st.TTShortcut++
				ai.stack[ply].pv[0] = te.m
				return ai.stack[ply].pv[:1], te.value
			}
			te = nil
		}
	}

	if ai.nullMoveOK(ply, depth, p) {
		ai.stack[ply].m = tak.Move{Type: tak.Pass}
		child, e := p.MovePreallocated(&ai.stack[ply].m, ai.stack[ply].p)
		if e == nil {
			ai.st.NullSearch++
			_, v := ai.zwSearch(child, ply+1, depth-3, nil, -α-1, true)
			v = -v
			if v >= α+1 {
				ai.st.NullCut++
				return nil, v
			}
		}
	}

	if !ai.cfg.NoReduceSlides && ply > 0 {
		m := ai.stack[ply-1].m
		if m.IsSlide() && len(m.Slides) == 1 {
			i := m.X + m.Y*int(ai.c.Size)
			dx, dy := m.Dest()
			j := dx + dy*int(ai.c.Size)
			if p.Height[i] == 0 && p.Height[j] == m.Slides[0] {
				ai.st.ReducedSlides++
				depth -= 2
			}
		}
	}

	mg := &ai.stack[ply].mg
	*mg = moveGenerator{
		ai:    ai,
		ply:   ply,
		depth: depth,
		p:     p,
		te:    te,
		pv:    pv,
	}

	var i int

	if cut && depth > 3 && !ai.cfg.NoMultiCut {
		cuts := 0
		ai.st.MCSearch++
		for _, child := mg.Next(); child != nil && i < multiCutSearch; _, child = mg.Next() {
			i++
			_, v := ai.zwSearch(child, ply+1, depth-1-2, nil, -α-1, !cut)
			if -v > α {
				cuts++
				if cuts >= multiCutThreshold {
					ai.st.MCCut++
					return nil, α + 1
				}
			}
		}
	}

	mg.Reset()
	i = 0

	best := ai.stack[ply].pv[:0]
	best = append(best, pv...)
	var didCut bool
	for m, child := mg.Next(); child != nil; m, child = mg.Next() {
		i++
		var ms []tak.Move
		var newpv []tak.Move
		var v int64
		if len(best) != 0 {
			newpv = best[1:]
		}
		ai.stack[ply].m = m
		ms, v = ai.zwSearch(child, ply+1, depth-1, newpv, -α-1, !cut)
		v = -v

		if len(best) == 0 {
			best = append(best[:0], m)
			best = append(best, ms...)
		}
		if v > α {
			best = append(best[:0], m)
			best = append(best, ms...)
			didCut = true
			ai.recordCut(&m, i, depth, ply)
			break
		}
		if atomic.LoadInt32(ai.cancel) != 0 {
			return nil, 0
		}
	}

	if te = ai.ttPut(p.Hash()); te != nil {
		te.hash = p.Hash()
		te.depth = depth
		te.m = best[0]
		te.value = α
		if didCut {
			te.bound = lowerBound
		} else {
			te.bound = upperBound
			ai.st.AllNodes++
		}
	}

	if didCut {
		return best, α + 1
	}
	return best, α
}

func (ai *MinimaxAI) nullMoveOK(ply, depth int, p *tak.Position) bool {
	if ai.cfg.NoNullMove {
		return false
	}
	if ply == 0 || depth < 3 {
		return false
	}
	if ai.stack[ply-1].m.Type == tak.Pass {
		return false
	}
	if p.WhiteStones() < 3 || p.BlackStones() < 3 {
		return false
	}
	if bitboard.Popcount(p.White|p.Black)+3 >= len(p.Stacks) {
		return false
	}
	return true
}
