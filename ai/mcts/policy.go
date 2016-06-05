package mcts

import (
	"math/rand"

	"github.com/nelhage/taktician/ai"
	"github.com/nelhage/taktician/tak"
)

func RandomPolicy(r *rand.Rand, p *tak.Position, alloc *tak.Position) *tak.Position {
	moves := p.AllMoves(nil)
	var next *tak.Position
	for {
		r := r.Int31n(int32(len(moves)))
		m := moves[r]
		var e error
		if next, e = p.MovePreallocated(&m, alloc); e == nil {
			break
		}
		moves[0], moves[r] = moves[r], moves[0]
		moves = moves[1:]
	}
	return next
}

func NewMinimaxPolicy(cfg *MCTSConfig, depth int) PolicyFunc {
	mm := ai.NewMinimax(ai.MinimaxConfig{
		Size:    cfg.Size,
		NoTable: true,
		Depth:   depth,
		Seed:    cfg.Seed,
	})
	return func(r *rand.Rand, p *tak.Position, next *tak.Position) *tak.Position {
		m := mm.GetMove(p, 0)
		next, _ = p.MovePreallocated(&m, next)
		return next
	}
}
