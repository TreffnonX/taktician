package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime/pprof"
	"time"

	"golang.org/x/net/context"

	"github.com/nelhage/taktician/ai"
	"github.com/nelhage/taktician/cli"
	"github.com/nelhage/taktician/ptn"
	"github.com/nelhage/taktician/tak"
)

var (
	all     = flag.Bool("all", false, "show all possible moves")
	tps     = flag.Bool("tps", false, "render position in tps")
	quiet   = flag.Bool("quiet", false, "don't print board diagrams")
	explain = flag.Bool("explain", false, "explain scoring")
	eval    = flag.Bool("evaluate", false, "only show static evaluation")

	move  = flag.Int("move", 0, "PTN move number to analyze")
	final = flag.Bool("final", false, "analyze final position only")
	black = flag.Bool("black", false, "only analyze black's move")
	white = flag.Bool("white", false, "only analyze white's move")

	debug     = flag.Int("debug", 1, "debug level")
	depth     = flag.Int("depth", 0, "minimax depth")
	timeLimit = flag.Duration("limit", time.Minute, "limit of how much time to use")

	seed         = flag.Int64("seed", 0, "specify a seed")
	sort         = flag.Bool("sort", true, "sort moves via history heuristic")
	table        = flag.Bool("table", true, "use the transposition table")
	nullMove     = flag.Bool("null-move", true, "use null-move pruning")
	extendForces = flag.Bool("extend-forces", true, "extend forced moves")

	cpuProfile = flag.String("cpuprofile", "", "write CPU profile")

	weights = flag.String("weights", "", "JSON-encoded evaluation weights")
)

func main() {
	flag.Parse()

	f, e := os.Open(flag.Arg(0))
	if e != nil {
		log.Fatal("open:", e)
	}
	parsed, e := ptn.ParsePTN(f)
	if e != nil {
		log.Fatal("parse:", e)
	}
	color := tak.NoColor
	switch {
	case *white && *black:
		log.Fatal("-white and -black are exclusive")
	case *white:
		color = tak.White
	case *black:
		color = tak.Black
	case *move != 0:
		color = tak.White
	}

	if *cpuProfile != "" {
		f, e := os.OpenFile(*cpuProfile, os.O_WRONLY|os.O_CREATE, 0644)
		if e != nil {
			log.Fatalf("open cpu-profile: %s: %v", *cpuProfile, e)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	if *move != 0 || *final {
		p, e := parsed.PositionAtMove(*move, color)
		if e != nil {
			log.Fatal("find move:", e)
		}

		analyze(p)
	} else {
		p, e := parsed.InitialPosition()
		if e != nil {
			log.Fatal("initial:", e)
		}
		w, b := makeAI(p), makeAI(p)
		for _, o := range parsed.Ops {
			m, ok := o.(*ptn.Move)
			if !ok {
				continue
			}
			switch {
			case p.ToMove() == tak.White && color != tak.Black:
				fmt.Printf("%d. %s\n", p.MoveNumber()/2+1, ptn.FormatMove(&m.Move))
				analyzeWith(w, p)
			case p.ToMove() == tak.Black && color != tak.White:
				fmt.Printf("%d. ... %s\n", p.MoveNumber()/2+1, ptn.FormatMove(&m.Move))
				analyzeWith(b, p)
			}
			var e error
			p, e = p.Move(&m.Move)
			if e != nil {
				log.Fatalf("illegal move %s: %v",
					ptn.FormatMove(&m.Move), e)
			}
		}
	}
}

func makeAI(p *tak.Position) *ai.MinimaxAI {
	var w ai.Weights
	if *weights == "" {
		w = ai.DefaultWeights[p.Size()]
	} else {
		e := json.Unmarshal([]byte(*weights), &w)
		if e != nil {
			log.Fatalf("parse weights: %v", e)
		}
	}
	return ai.NewMinimax(ai.MinimaxConfig{
		Size:  p.Size(),
		Depth: *depth,
		Seed:  *seed,
		Debug: *debug,

		NoSort:         !*sort,
		NoTable:        !*table,
		NoNullMove:     !*nullMove,
		NoExtendForces: !*extendForces,

		Evaluate: ai.MakeEvaluator(p.Size(), &w),
	})
}

func analyze(p *tak.Position) {
	analyzeWith(makeAI(p), p)
}

func analyzeWith(player *ai.MinimaxAI, p *tak.Position) {
	if *eval {
		val := player.Evaluate(p)
		if p.ToMove() == tak.Black {
			val = -val
		}
		fmt.Printf(" Val=%d\n", val)
		if *explain {
			ai.ExplainScore(player, os.Stdout, p)
		}
		return
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(*timeLimit))
	defer cancel()
	pv, val, _ := player.Analyze(ctx, p)
	if !*quiet {
		cli.RenderBoard(os.Stdout, p)
		if *explain {
			ai.ExplainScore(player, os.Stdout, p)
		}
	}
	fmt.Printf("AI analysis:\n")
	fmt.Printf(" pv=")
	for _, m := range pv {
		fmt.Printf("%s ", ptn.FormatMove(&m))
	}
	fmt.Printf("\n")
	fmt.Printf(" value=%d\n", val)
	if *tps {
		fmt.Printf("[TPS \"%s\"]\n", ptn.FormatTPS(p))
	}
	if *all {
		fmt.Printf(" all moves:")
		for _, m := range p.AllMoves(nil) {
			fmt.Printf(" %s", ptn.FormatMove(&m))
		}
		fmt.Printf("\n")
	}
	fmt.Println()

	for _, m := range pv {
		n, e := p.Move(&m)
		if e != nil {
			log.Printf("illegal move in pv: %s: %v", ptn.FormatMove(&m), e)
			if val < ai.WinThreshold && val > -ai.WinThreshold {
				log.Fatal("illegal move in non-terminal pv!")
			}
			return
		}
		p = n
	}

	if !*quiet {
		fmt.Println("Resulting position:")
		cli.RenderBoard(os.Stdout, p)
		if *explain {
			ai.ExplainScore(player, os.Stdout, p)
		}
		fmt.Println()
		fmt.Println()
	}
}
