package tak

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/nelhage/taktician/bitboard"
)

type Config struct {
	Size      int
	Pieces    int
	Capstones int

	c bitboard.Constants
}

var defaultPieces = []int{0, 0, 0, 10, 15, 21, 30, 40, 50}
var defaultCaps = []int{0, 0, 0, 0, 0, 1, 1, 1, 2}

const (
	offset64 = 14695981039346656037
	prime64  = 1099511628211
)

var posHashes []uint64

func init() {
	for i := 0; i < 64; i++ {
		var b [8]byte
		_, e := rand.Read(b[:])
		if e != nil {
			panic(fmt.Sprintf("rand: %v", e))
		}
		posHashes = append(posHashes, binary.BigEndian.Uint64(b[:]))
	}
}

func New(g Config) *Position {
	if g.Pieces == 0 {
		g.Pieces = defaultPieces[g.Size]
	}
	if g.Capstones == 0 {
		g.Capstones = defaultCaps[g.Size]
	}
	g.c = bitboard.Precompute(uint(g.Size))
	p := &Position{
		cfg:         &g,
		whiteStones: byte(g.Pieces),
		whiteCaps:   byte(g.Capstones),
		blackStones: byte(g.Pieces),
		blackCaps:   byte(g.Capstones),
		move:        0,
		board:       make([]Square, g.Size*g.Size),
	}
	return p
}

type Square []Piece

type Position struct {
	cfg         *Config
	whiteStones byte
	whiteCaps   byte
	blackStones byte
	blackCaps   byte

	move     int
	board    []Square
	analysis Analysis
	hash     uint64
}

type Analysis struct {
	WhiteRoad   uint64
	BlackRoad   uint64
	White       uint64
	Black       uint64
	WhiteGroups []uint64
	BlackGroups []uint64
}

// FromSquares initializes a Position with the specified squares and
// move number. `board` is a slice of rows, numbered from low to high,
// each of which is a slice of positions.
func FromSquares(cfg Config, board [][]Square, move int) (*Position, error) {
	p := New(cfg)
	p.move = move
	for x := 0; x < p.Size(); x++ {
		for y := 0; y < p.Size(); y++ {
			p.set(x, y, board[y][x])
			for _, piece := range board[y][x] {
				switch piece {
				case MakePiece(White, Capstone):
					p.whiteCaps--
				case MakePiece(Black, Capstone):
					p.blackCaps--
				case MakePiece(White, Flat), MakePiece(White, Standing):
					p.whiteStones--
				case MakePiece(Black, Flat), MakePiece(Black, Standing):
					p.blackStones--
				default:
					return nil, errors.New("bad stone")
				}
			}
		}
	}
	p.analyze()
	return p, nil
}

func (p *Position) Size() int {
	return p.cfg.Size
}

func (p *Position) At(x, y int) Square {
	return p.board[y*p.cfg.Size+x]
}

func (p *Position) set(x, y int, s Square) {
	i := y*p.cfg.Size + x
	p.hash ^= p.hashAt(i)
	p.board[i] = s
	p.hash ^= p.hashAt(i)
}

func (p *Position) hashAt(i int) uint64 {
	if len(p.board[i]) == 0 {
		return 0
	}
	s := posHashes[i]
	for _, c := range p.board[i] {
		s ^= uint64(c)
		s *= prime64
	}
	return s
}

func (p *Position) Hash() uint64 {
	return p.hash
}

func (p *Position) ToMove() Color {
	if p.move%2 == 0 {
		return White
	}
	return Black
}

func (p *Position) MoveNumber() int {
	return p.move
}

func (p *Position) WhiteStones() int {
	return int(p.whiteStones)
}

func (p *Position) BlackStones() int {
	return int(p.blackStones)
}

func (p *Position) GameOver() (over bool, winner Color) {
	if p, ok := p.hasRoad(); ok {
		return true, p
	}

	if (p.whiteStones+p.whiteCaps) != 0 &&
		(p.blackStones+p.blackCaps) != 0 &&
		(p.analysis.White|p.analysis.Black) != p.cfg.c.Mask {
		return false, NoColor
	}

	return true, p.flatsWinner()
}

func (p *Position) roadAt(x, y int) (Color, bool) {
	sq := p.At(x, y)
	if len(sq) == 0 {
		return White, false
	}
	return sq[0].Color(), sq[0].IsRoad()
}

func (p *Position) hasRoad() (Color, bool) {
	white, black := false, false

	for _, g := range p.analysis.WhiteGroups {
		if ((g&p.cfg.c.T) != 0 && (g&p.cfg.c.B) != 0) ||
			((g&p.cfg.c.L) != 0 && (g&p.cfg.c.R) != 0) {
			white = true
			break
		}
	}
	for _, g := range p.analysis.BlackGroups {
		if ((g&p.cfg.c.T) != 0 && (g&p.cfg.c.B) != 0) ||
			((g&p.cfg.c.L) != 0 && (g&p.cfg.c.R) != 0) {
			black = true
			break
		}
	}

	switch {
	case white && black:
		if p.ToMove() == White {
			return Black, true
		}
		return White, true
	case white:
		return White, true
	case black:
		return Black, true
	default:
		return White, false
	}

}

func (p *Position) Analysis() *Analysis {
	return &p.analysis
}

func (p *Position) analyze() {
	var br uint64
	var wr uint64
	var b uint64
	var w uint64
	for i, sq := range p.board {
		if len(sq) == 0 {
			continue
		}
		if sq[0].Color() == White {
			w |= 1 << uint(i)
		} else {
			b |= 1 << uint(i)
		}
		if sq[0].IsRoad() {
			if sq[0].Color() == White {
				wr |= 1 << uint(i)
			} else {
				br |= 1 << uint(i)
			}
		}
	}
	p.analysis.WhiteRoad = wr
	p.analysis.BlackRoad = br
	p.analysis.White = w
	p.analysis.Black = b

	alloc := make([]uint64, 0, 2*p.Size())
	p.analysis.WhiteGroups = bitboard.FloodGroups(&p.cfg.c, wr, alloc)
	alloc = p.analysis.WhiteGroups
	alloc = alloc[len(alloc):len(alloc):cap(alloc)]
	p.analysis.BlackGroups = bitboard.FloodGroups(&p.cfg.c, br, alloc)
}

func (p *Position) countFlats() (w int, b int) {
	cw, cb := 0, 0
	for i := 0; i < p.cfg.Size*p.cfg.Size; i++ {
		stack := p.board[i]
		if len(stack) > 0 {
			if stack[0].Kind() == Flat {
				if stack[0].Color() == White {
					cw++
				} else {
					cb++
				}
			}
		}
	}
	return cw, cb
}

func (p *Position) flatsWinner() Color {
	cw, cb := p.countFlats()
	if cw > cb {
		return White
	}
	if cb > cw {
		return Black
	}
	return NoColor
}

type WinReason int

const (
	RoadWin WinReason = iota
	FlatsWin
	Resignation
)

type WinDetails struct {
	Over       bool
	Reason     WinReason
	Winner     Color
	WhiteFlats int
	BlackFlats int
}

func (p *Position) WinDetails() WinDetails {
	over, c := p.GameOver()
	var d WinDetails
	d.Over = over
	d.Winner = c
	d.WhiteFlats, d.BlackFlats = p.countFlats()
	if _, ok := p.hasRoad(); ok {
		d.Reason = RoadWin
	} else {
		d.Reason = FlatsWin
	}
	return d
}
