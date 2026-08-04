package gcdiff

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Reading the reasons out of cmd/compile's -m=2, and out of goc's -m.
//
// # Why gc's reasons need a second build
//
// -m prints placements. It prints no reason for any of them: `x escapes to
// heap` and `x does not escape` are verdicts, and the only reason-shaped things
// at level 1 are `leaking param`, which is a fact about a parameter rather than
// about an allocation. The explanations are at -m=2, which prints an extra
// block per escaping object:
//
//	./p.go:9:27: &bytes.Reader{...} escapes to heap in main:
//	./p.go:9:27:   flow: {heap} ← &{storage for &bytes.Reader{...}}:
//	./p.go:9:27:     from &bytes.Reader{...} (spill) at ./p.go:9:27
//	./p.go:9:27:     from keep(&bytes.Reader{...}) (call parameter) at ./p.go:9:27
//
// and then repeats the level-1 verdict lines below them.
//
// The reasons are therefore read from a second build, at -m=2, and joined onto
// the decisions the existing level-1 parse already produced. That is a
// deliberate cost: parsing the placements out of -m=2 instead would be one
// build rather than two, and would put the committed placement differential --
// which is a baseline several jobs have already read and accepted -- at the
// mercy of a parser change. ParseGCFlagM is not touched by any of this, and the
// placement side of the differential is byte-for-byte the analysis it was.
//
// # The join between the two builds
//
// (line, column, subject). All three are printed identically by both levels,
// and together they identify the decision within the program: two allocations
// on one line have different columns, and the pathological case of two
// different subjects at one position is separated by the subject. Explanations
// that match no level-1 decision are counted, not dropped -- see
// Coverage.UnmatchedGCExplanations.

// gcExplained matches the header of an explained block: a decision line with a
// containing function and a trailing colon, which is what tells it apart from
// the level-1 verdict line for the same decision.
var gcExplained = regexp.MustCompile(`^(.*) (escapes to heap) in (\S+):$`)

// gcFlowLine matches a flow group header: `  flow: DEST ← SOURCE:`.
var gcFlowLine = regexp.MustCompile(`^ *flow: (.*) ← (.*):$`)

// gcFromLine matches one edge: `    from EXPR (KIND) at POS`. The position is
// optional in form -- gc prints `<unknown line number>` for a closure variable
// it has no position for -- so the kind is anchored on the parenthesised group
// immediately before " at ", which is the last one on the line.
var gcFromLine = regexp.MustCompile(`^ *from .*\(([^()]*)\) at \S.*$`)

// ExplanationKey identifies a decision within one program, for joining the
// level-2 explanation onto the level-1 decision.
type ExplanationKey struct {
	Line, Col int
	Subject   string
}

// GCExplanations is what ParseGCExplanations got out of one program's -m=2.
type GCExplanations struct {
	// Program is the corpus program's base name.
	Program string
	// Flows maps a decision to its explanation. A position with two explained
	// blocks for one subject -- the same allocation explained once per function
	// it was inlined into -- keeps the first, since the join key deliberately
	// does not carry the function.
	Flows map[ExplanationKey]GCFlow
	// Blocks is how many explained blocks were found at positions inside
	// Program, including any dropped as duplicates.
	Blocks int
	// Foreign is explained blocks at a position outside Program.
	Foreign int
}

// ParseGCExplanations reads the stderr of `go build -gcflags=-m=2 program` and
// returns the explained escape blocks for positions inside program.
//
// It deliberately ignores everything that is not an explained block. -m=2 also
// prints inlining costs, closure capture lists and per-parameter leak
// summaries, none of which is an allocation's reason, and the level-1 verdict
// lines, which the level-1 parse has already read. Being permissive here is
// safe in a way it is not in ParseGCFlagM: an unrecognised line there removes a
// decision from one side of the placement matrix, where an unrecognised line
// here can only fail to attach a reason -- and every decision with no reason
// attached is counted and reported.
func ParseGCExplanations(program, output string) (GCExplanations, error) {
	explanations := GCExplanations{Program: program, Flows: make(map[ExplanationKey]GCFlow)}

	var current *GCFlow
	var key ExplanationKey
	var inProgram bool
	commit := func() {
		if current == nil {
			return
		}
		if inProgram {
			explanations.Blocks++
			if _, seen := explanations.Flows[key]; !seen {
				explanations.Flows[key] = *current
			}
		} else {
			explanations.Foreign++
		}
		current = nil
	}

	for _, text := range strings.Split(output, "\n") {
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		match := gcPosition.FindStringSubmatch(text)
		if match == nil {
			commit()
			continue
		}
		file, message := match[1], match[4]
		line, _ := strconv.Atoi(match[2])
		col, _ := strconv.Atoi(match[3])

		if header := gcExplained.FindStringSubmatch(message); header != nil {
			commit()
			inProgram = strings.TrimPrefix(file, "./") == program
			key = ExplanationKey{Line: line, Col: col, Subject: header[1]}
			current = &GCFlow{Func: header[3], Text: message}
			continue
		}
		if current == nil {
			continue
		}
		if flow := gcFlowLine.FindStringSubmatch(message); flow != nil {
			current.Dest, current.Source = flow[1], flow[2]
			current.Text += "\n" + message
			continue
		}
		if from := gcFromLine.FindStringSubmatch(message); from != nil {
			current.Edges = append(current.Edges, from[1])
			current.Text += "\n" + message
			continue
		}
		// Any other diagnostic at any position ends the block: -m=2 prints an
		// explained block's continuations immediately after its header and
		// nothing else in between.
		commit()
	}
	commit()
	return explanations, nil
}

// gocDecision matches goc's decision line, which is worded exactly like gc's.
// The position may be "?" for a site the front end gave no position.
var gocDecision = regexp.MustCompile(`^(\?|.*:\d+:\d+): (.*) (does not escape|escapes to heap)$`)

var gocPosition = regexp.MustCompile(`^(.*):(\d+):(\d+)$`)

// GocSite is one decision from goc's -m, with its explanation.
type GocSite struct {
	// File is the program's base name, Line and Col the position.
	File      string
	Line, Col int
	// Placer is "front end" or "ir pass": which of goc's two placers decided.
	Placer string
	// Site is the construct the placer recorded, Subject what the decision line
	// named -- the type-descriptor symbol, rendered as the allocation census
	// renders it.
	Site    string
	Func    string
	Subject string
	// Placement is what goc decided.
	Placement Placement
	// Rule is the rule that decided, verbatim. Empty for a frame placement.
	Rule string
	// Reason is Rule's category.
	Reason Reason
	// Chain is the `from:` lines and Use the `at:` line, at level 2.
	Chain []string
	Use   string
}

// GocReport is what ParseGocFlagM got out of one program's -m.
type GocReport struct {
	// Program is the corpus program's base name.
	Program string
	// Sites are the decisions at positions inside Program.
	Sites []GocSite
	// Positionless is decisions goc printed with no source position. They cannot
	// join and are counted rather than dropped: an allocation goc's lowering
	// creates rather than the source asks for carries no position, and the
	// census has the same blind spot from the same cause.
	Positionless int
	// Foreign is decisions at a position outside Program. WriteEscapeDiagnostics
	// already restricts its report to the compiled file, so this should be zero;
	// it is counted so that a change to that restriction is visible rather than
	// silent.
	Foreign int
	// UncategorisedRules is every rule string ClassifyGocRule did not recognise,
	// with its position, for reporting.
	UncategorisedRules []string
	// Unknown is every line the parser did not recognise at all. As with
	// GCReport.Unknown it must be empty: goc's -m is this tree's own output, so
	// a line this parser cannot read is a change to the diagnostic that the
	// differential has not been taught.
	Unknown []string
}

// ParseGocFlagM reads goc's -m report -- the bytes opt.WriteEscapeDiagnostics
// writes -- and returns the decisions it printed for positions inside program.
//
// program is the base name, and a decision's file is matched on its base name
// too, because the compiler is pointed at a path (goc/testdata/x.go) and the
// join names a program (x.go).
//
// The shape it reads is the one docs/escape-diagnostics.md documents: an
// unindented decision line worded as gc words it, followed by tab-indented
// continuations. The continuations are what ParseGCFlagM skips, which is what
// let goc's output be checked against the strict gc parser before this existed;
// this parser is the one that keeps them.
func ParseGocFlagM(program, output string) (GocReport, error) {
	report := GocReport{Program: program}
	var current *GocSite
	// dropped is set when the decision line just read was one this join cannot
	// use -- positionless, or in another file. Its continuation lines are then
	// skipped rather than reported as unreadable.
	dropped := false
	commit := func() {
		if current == nil {
			return
		}
		reason, known := ClassifyGocRule(current.Rule)
		current.Reason = reason
		if !known {
			report.UncategorisedRules = append(report.UncategorisedRules,
				fmt.Sprintf("%s:%d:%d: %s", current.File, current.Line, current.Col, current.Rule))
		}
		report.Sites = append(report.Sites, *current)
		current = nil
	}

	for _, text := range strings.Split(output, "\n") {
		if text == "" {
			continue
		}
		if strings.HasPrefix(text, "\t") {
			if current == nil {
				if !dropped {
					report.Unknown = append(report.Unknown, text)
				}
				continue
			}
			detail := strings.TrimPrefix(text, "\t")
			switch {
			case strings.HasPrefix(detail, "front end: "), strings.HasPrefix(detail, "ir pass: "):
				placer, rest, _ := strings.Cut(detail, ": ")
				current.Placer = placer
				site, function, _ := strings.Cut(rest, " in ")
				current.Site, current.Func = site, function
			case strings.HasPrefix(detail, "rule: "):
				current.Rule = strings.TrimPrefix(detail, "rule: ")
			case strings.HasPrefix(detail, "from: "):
				current.Chain = append(current.Chain, strings.TrimPrefix(detail, "from: "))
			case strings.HasPrefix(detail, "at:"):
				current.Use = strings.TrimSpace(strings.TrimPrefix(detail, "at:"))
			default:
				report.Unknown = append(report.Unknown, text)
			}
			continue
		}

		commit()
		dropped = false
		match := gocDecision.FindStringSubmatch(text)
		if match == nil {
			report.Unknown = append(report.Unknown, text)
			continue
		}
		placement := Frame
		if match[3] == "escapes to heap" {
			placement = Heap
		}
		if match[1] == "?" {
			report.Positionless++
			dropped = true
			continue
		}
		position := gocPosition.FindStringSubmatch(match[1])
		if position == nil {
			report.Unknown = append(report.Unknown, text)
			continue
		}
		if baseName(position[1]) != program {
			report.Foreign++
			dropped = true
			continue
		}
		site := GocSite{File: program, Subject: match[2], Placement: placement}
		site.Line, _ = strconv.Atoi(position[2])
		site.Col, _ = strconv.Atoi(position[3])
		current = &site
	}
	commit()
	return report, nil
}

// baseName is filepath.Base for the forward-slash paths goc prints, without
// importing filepath into a package that otherwise touches no filesystem.
func baseName(path string) string {
	if cut := strings.LastIndexByte(path, '/'); cut >= 0 {
		return path[cut+1:]
	}
	return path
}
