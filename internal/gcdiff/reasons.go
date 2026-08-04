package gcdiff

import (
	"regexp"
	"sort"
	"strings"
)

// The reason taxonomy: what the two compilers are asked to agree about beyond
// where the object went.
//
// # Why a taxonomy and not the strings
//
// goc and cmd/compile answer "why is this on the heap" in different
// vocabularies, describing different analyses. goc says `argument 0 of
// $runtime.newproc escapes` and `write barrier into a candidate`; gc says
// `from func literal (spill)` under `flow: {heap} <- &{storage for func
// literal}` and `from p.q = x (assign)`. Neither is a translation of the other
// and a differential that compared the strings would report a disagreement on
// every joined line, which is the same as reporting nothing.
//
// Worse, gc's wording is not stable across releases -- the committed
// differential already records the host toolchain for exactly that reason --
// and reason wording is far more exposed to that than placement wording is.
// `escapes to heap` has survived a decade; the flow-edge vocabulary is internal
// notation that gains and loses members between releases.
//
// So both sides are normalised into a small set of categories that name the
// *mechanism* by which an object outlives its frame, and the comparison is
// between categories. The raw string is kept beside the category on every
// decision, because the category is what is comparable and the string is what
// is diagnosable.
//
// # What the categories are
//
// They are mechanisms, not analyses. A category exists when the two compilers
// could in principle both name the same thing, or -- for the four that only one
// side can produce -- when the asymmetry is itself the finding.
//
// # The one translation that is not a lookup
//
// goc lowers three language constructs to calls into the runtime before its
// escape analysis runs, so its reason for them names the runtime entry point:
// `go f(x)` becomes an argument of $runtime.newproc, `defer f(x)` an argument
// of $runtime.deferproc, and `ch <- x` an argument of $runtime.chansend. gc's
// analysis runs on a tree that still has the construct, so it says `captured by
// a closure` and `send`. Mapping goc's three runtime callees back to the
// construct they implement is a translation between the vocabularies, not a
// fudge: without it 111 corpus sites where the two compilers agree completely
// would be reported as disagreeing about a call.
//
// runtimeConstructCallees below is the whole of that translation, and it is
// deliberately a closed list of three: a runtime entry point not on it is a
// call like any other and is categorised as one.

// Reason is the normalised category of an escape explanation.
//
// The empty Reason means no explanation was given, which for both compilers is
// every frame placement: a frame placement is the absence of a publication
// rather than the presence of a rule, and neither -m nor goc -m says anything
// about one.
type Reason string

const (
	// ReasonCallRetains is: handed to a call that may keep it. The single
	// largest category on both sides.
	ReasonCallRetains Reason = "call-retains"
	// ReasonCallOpaque is: handed to a call the analysis could not see through
	// -- no body in this compilation, no declaration, an unresolved indirect
	// target, or a recursion the walk cut by answering "escapes".
	//
	// gc cannot produce this. Its escape analysis is complete over the package
	// it compiles plus the summaries in export data, so it never has to answer
	// "I could not tell" -- where it cannot see, it has a fact instead. Every
	// line in this category is therefore a line where goc reached the same
	// placement without the same knowledge.
	ReasonCallOpaque Reason = "call-opaque"
	// ReasonStoredInObject is: the address was written into storage that is not
	// a frame slot -- a package-level variable, a field of another object, an
	// element of a container.
	ReasonStoredInObject Reason = "stored-in-object"
	// ReasonReturned is: it leaves through one of the function's results.
	ReasonReturned Reason = "returned"
	// ReasonInterfaceBoxed is: it was converted to an interface, and the payload
	// needs storage the interface word can point at.
	ReasonInterfaceBoxed Reason = "interface-boxed"
	// ReasonClosureCaptured is: captured by a closure, a goroutine or a defer.
	ReasonClosureCaptured Reason = "closure-captured"
	// ReasonChannelSend is: sent on a channel.
	ReasonChannelSend Reason = "channel-send"
	// ReasonLoopCarried is: one frame slot cannot hold one object per iteration.
	//
	// gc cannot produce this either, and for a more interesting reason than
	// call-opaque: gc has no such rule, because it does not need one. A loop
	// body's allocation stays in the frame unless something publishes it, and
	// the per-iteration copy a range loop needs is a separate variable gc
	// promotes on its own terms. goc's rule is a blanket one.
	ReasonLoopCarried Reason = "loop-carried"
	// ReasonReadOut is: the object's contents are read back out of the container
	// holding them, so the container's storage cannot be reused.
	ReasonReadOut Reason = "read-out"
	// ReasonTooLarge is: it will not fit in a frame. gc-only; goc has no size
	// rule and no reason string for one.
	ReasonTooLarge Reason = "too-large"
	// ReasonFolded is: the analysis stopped deciding this allocation on its own
	// and folded it in with others -- a phi that merges two allocations, or a
	// container that holds more payloads than it costs to hold. goc-only: it
	// describes a shortcut in goc's own analysis rather than anything the
	// program does.
	ReasonFolded Reason = "folded"
	// ReasonUnexplained is: the analysis reached a use it could not name. goc
	// prints a rule saying so rather than printing nothing, which is the right
	// choice -- but it is not an explanation, and counting it as one would make
	// the two compilers look like they agree when one of them has said nothing.
	// goc-only.
	ReasonUnexplained Reason = "unexplained"
	// ReasonUncategorised is a reason string neither classifier recognised. It
	// is a gap in this instrument, not a property of the program, and Coverage
	// reports it with examples for exactly that reason.
	ReasonUncategorised Reason = "uncategorised"
)

// Reasons is the order reason categories are printed in: most mechanical first,
// the two "the analysis gave up" categories last, and the gap last of all.
var Reasons = []Reason{
	ReasonCallRetains,
	ReasonStoredInObject,
	ReasonInterfaceBoxed,
	ReasonClosureCaptured,
	ReasonReturned,
	ReasonChannelSend,
	ReasonReadOut,
	ReasonTooLarge,
	ReasonLoopCarried,
	ReasonFolded,
	ReasonCallOpaque,
	ReasonUnexplained,
	ReasonUncategorised,
}

// OnlyGoc and OnlyGC are the categories one compiler can produce and the other
// structurally cannot. A line whose two categories are one of these is not the
// two compilers disagreeing about the program; it is one of them describing its
// own analysis. Reported separately for that reason.
var (
	OnlyGoc = map[Reason]bool{
		ReasonCallOpaque:  true,
		ReasonLoopCarried: true,
		ReasonFolded:      true,
		ReasonUnexplained: true,
	}
	OnlyGC = map[Reason]bool{
		ReasonTooLarge: true,
	}
)

// runtimeConstructCallees maps the runtime entry points goc lowers a language
// construct to, back to the construct. See the package comment above.
var runtimeConstructCallees = map[string]Reason{
	"runtime.newproc":   ReasonClosureCaptured,
	"runtime.deferproc": ReasonClosureCaptured,
	"runtime.chansend":  ReasonChannelSend,
	"runtime.chansend1": ReasonChannelSend,
}

// gocArgumentRule matches the IR pass's four call-argument rules, capturing the
// callee so runtimeConstructCallees can be consulted.
var gocArgumentRule = regexp.MustCompile(`^argument \d+ of \$([^ ]+) (escapes|may retain something it points at|may retain something inside a self-referential object|leaks to a result the caller cannot follow)$`)

// gocCallRule matches the IR pass's "call to $F" family.
var gocCallRule = regexp.MustCompile(`^call to \$([^,]+?)(,.*| with misaligned scalarised aggregate arguments)?$`)

// gocRulePrefixes are the AST walk's rules, matched on a prefix because each
// one interpolates a name.
//
// The list is closed on purpose. A rule the walk gains and this table has not
// been taught comes out ReasonUncategorised and is reported with an example,
// which is the behaviour ParseGCFlagM already has for an unknown -m diagnostic
// and for the same reason: a reason silently dropped is a reason that reads as
// agreement.
var gocRulePrefixes = []struct {
	prefix string
	reason Reason
}{
	{"passed to a call the walk cannot resolve to a single function", ReasonCallOpaque},
	{"passed in the variadic position of ", ReasonCallRetains},
	{"passed to ", ""}, // resolved by classifyGocPassedTo, below.
	{"used as the receiver of ", ReasonCallRetains},
	{"assigned to the package-level variable ", ReasonStoredInObject},
	{"converted to ", ReasonInterfaceBoxed},
	{"returned", ReasonReturned},
	{"store into non-local storage", ReasonStoredInObject},
	{"write barrier into non-local storage", ReasonStoredInObject},
	{"write barrier into a candidate", ReasonStoredInObject},
	{"read back out of the object holding it", ReasonReadOut},
	{"block-copied out of the object holding it", ReasonReadOut},
	{"allocated in a loop", ReasonLoopCarried},
	{"its address is still reachable on the next iteration", ReasonLoopCarried},
	{"holds more separately-allocated payloads", ReasonFolded},
	{"phi", ReasonFolded},
	{"indirect call", ReasonCallOpaque},
	{"callee of an indirect call", ReasonCallOpaque},
	{"the walk found a use it could not prove local", ReasonUnexplained},
	{"the walk is already inside the question about ", ReasonUnexplained},
	{"used by ", ReasonUnexplained},
}

// gocPassedToOpaque are the tails of the walk's "passed to F, ..." rules that
// mean the walk could not see into the callee, as opposed to seeing into it and
// finding that it retains the argument.
var gocPassedToOpaque = []string{
	"whose declaration this compilation does not have",
	"which has no Go body and is not marked //go:noescape",
	"which the walk is already inside",
}

// ClassifyGocRule maps one of goc's rule strings to a category.
//
// The second result is false when nothing matched, which the caller must report
// rather than drop: the categories are what the differential compares, so a
// rule that does not map removes a line from the comparison and makes the two
// compilers look like they agree about it.
func ClassifyGocRule(rule string) (Reason, bool) {
	if rule == "" {
		return "", true
	}
	if match := gocArgumentRule.FindStringSubmatch(rule); match != nil {
		if reason, construct := runtimeConstructCallees[genericBase(match[1])]; construct {
			return reason, true
		}
		if match[2] == "leaks to a result the caller cannot follow" {
			// The callee puts it in a result the caller cannot track, which is
			// the callee retaining it as far as this call site can tell.
			return ReasonCallRetains, true
		}
		return ReasonCallRetains, true
	}
	if match := gocCallRule.FindStringSubmatch(rule); match != nil {
		if match[2] == "" {
			return ReasonCallRetains, true
		}
		return ReasonCallOpaque, true
	}
	for _, entry := range gocRulePrefixes {
		if !strings.HasPrefix(rule, entry.prefix) {
			continue
		}
		if entry.prefix == "passed to " {
			return classifyGocPassedTo(rule), true
		}
		return entry.reason, true
	}
	// The walk's fallback rule for an object it watched escape without being
	// able to name the use: "<name> is used here in a way the walk cannot prove
	// keeps it local". It starts with the object's name, so it cannot be matched
	// by a prefix.
	if strings.Contains(rule, " is captured by a function literal that escapes") {
		return ReasonClosureCaptured, true
	}
	if strings.Contains(rule, " is used here in a way the walk cannot prove keeps it local") ||
		strings.Contains(rule, " is declared outside the body the walk can see every use in") {
		return ReasonUnexplained, true
	}
	return ReasonUncategorised, false
}

func classifyGocPassedTo(rule string) Reason {
	for _, tail := range gocPassedToOpaque {
		if strings.Contains(rule, tail) {
			return ReasonCallOpaque
		}
	}
	return ReasonCallRetains
}

// genericBase strips a generic instantiation's type arguments from a symbol
// name, so that $runtime.AddCleanup[main.box,int] is recognised as
// runtime.AddCleanup. The construct callees are not generic, but the same
// stripping keeps a generic instantiation of an ordinary function from being
// three different callees in a report.
func genericBase(symbol string) string {
	if cut := strings.IndexByte(symbol, '['); cut >= 0 {
		return symbol[:cut]
	}
	return symbol
}

// gcFlowArtefacts are the flow edges that describe how an expression reaches
// its own storage rather than how that storage escapes. They are skipped when
// looking for the edge that decided.
//
// `spill` is the value being written to the storage that holds it; `address-of`
// is `&x`; `reference` is a closure's variable reference. Every explained block
// starts with one of them and none of them is a reason.
var gcFlowArtefacts = map[string]bool{
	"spill":      true,
	"address-of": true,
	"reference":  true,
}

// gcFlowEdges maps cmd/compile's flow-edge vocabulary to categories.
//
// Measured, not guessed: these are the 26 edge kinds `-m=2` printed over the
// whole corpus. A kind not listed comes out ReasonUncategorised and is reported,
// which is what will happen the first time this is run against a release that
// adds one.
var gcFlowEdges = map[string]Reason{
	"call parameter":         ReasonCallRetains,
	"interface-converted":    ReasonInterfaceBoxed,
	"captured by a closure":  ReasonClosureCaptured,
	"closures":               ReasonClosureCaptured,
	"closures, func literal": ReasonClosureCaptured,
	"func literal":           ReasonClosureCaptured,
	"return":                 ReasonReturned,
	"send":                   ReasonChannelSend,
	"too large for stack":    ReasonTooLarge,

	"assign":                 ReasonStoredInObject,
	"assign-pair":            ReasonStoredInObject,
	"assign-pair-dot-type":   ReasonStoredInObject,
	"struct literal element": ReasonStoredInObject,
	"slice-literal-element":  ReasonStoredInObject,
	"map literal value":      ReasonStoredInObject,
	"map literal key":        ReasonStoredInObject,
	"key of map put":         ReasonStoredInObject,
	"copied slice":           ReasonStoredInObject,
	"appendee slice":         ReasonStoredInObject,
	"fixed-array-index-of":   ReasonStoredInObject,
	"switch case":            ReasonStoredInObject,

	"dot":            ReasonReadOut,
	"dot of pointer": ReasonReadOut,
	"indirection":    ReasonReadOut,
	"range-deref":    ReasonReadOut,
}

// gcResult matches the name cmd/compile gives a function result in a flow
// destination: ~r0, or sync.~r0 for a result of an inlined callee.
var gcResult = regexp.MustCompile(`(^|\.)~r\d+$`)

// GCFlow is one explained escape from -m=2: the chain of flow groups between
// the object and whatever made it escape.
type GCFlow struct {
	// Func is the function the block was printed for, from "escapes to heap in
	// F:". Kept for triage; not part of the join, for the reason the package
	// comment gives.
	Func string
	// Edges is every flow edge in order, across every flow group.
	Edges []string
	// Dest and Source are the last flow group's two ends.
	Dest   string
	Source string
	// Text is the whole block as -m=2 printed it, minus the repeated position
	// prefix. It is what a person reads when a category is not enough.
	Text string
}

// Reason categorises the flow.
//
// The deciding edge is the last non-artefact edge anywhere in the chain, not
// the last edge of the last group: gc ends a chain that passes through a
// dereference with a group like `flow: {heap} <- *{temp}` that has no edges at
// all, and the edge that decided is the one before it. Over the corpus this
// recovers 20 of the 100 blocks whose final group is edgeless.
//
// With no non-artefact edge anywhere, the flow's own ends answer: a chain that
// runs from a func literal's storage straight to {heap} is a closure the
// compiler put on the heap, and a chain that ends at ~rN left through a result.
func (flow GCFlow) Reason() (Reason, bool) {
	for index := len(flow.Edges) - 1; index >= 0; index-- {
		edge := flow.Edges[index]
		if gcFlowArtefacts[edge] {
			continue
		}
		if reason, known := gcFlowEdges[edge]; known {
			return reason, true
		}
		return ReasonUncategorised, false
	}
	switch {
	case strings.Contains(flow.Source, "storage for func literal"):
		return ReasonClosureCaptured, true
	case gcResult.MatchString(flow.Dest):
		return ReasonReturned, true
	case strings.HasPrefix(flow.Dest, "{storage for "):
		return ReasonStoredInObject, true
	}
	return ReasonUncategorised, false
}

// ReasonSet is the set of categories one compiler gave for one source line.
//
// A line can carry several allocations and several explanations. Folding them
// to a set rather than to one value is forced by the same thing that forces the
// verdict fold: the join key is the line, and which explanation belongs to which
// allocation on a line carrying more than one is not something the join can
// know.
type ReasonSet map[Reason]bool

// Sorted returns the set's members in Reasons order, for stable output.
func (set ReasonSet) Sorted() []Reason {
	out := make([]Reason, 0, len(set))
	for _, reason := range Reasons {
		if set[reason] {
			out = append(out, reason)
		}
	}
	// A category not in Reasons should not exist, but sorting the remainder
	// keeps the output stable if one ever does.
	var extra []string
	for reason := range set {
		if !reasonKnown(reason) {
			extra = append(extra, string(reason))
		}
	}
	sort.Strings(extra)
	for _, reason := range extra {
		out = append(out, Reason(reason))
	}
	return out
}

// String renders the set as a stable comma-separated list, "-" when empty.
func (set ReasonSet) String() string {
	if len(set) == 0 {
		return "-"
	}
	names := make([]string, 0, len(set))
	for _, reason := range set.Sorted() {
		names = append(names, string(reason))
	}
	return strings.Join(names, ",")
}

// Intersects reports whether the two sets share a category.
func (set ReasonSet) Intersects(other ReasonSet) bool {
	for reason := range set {
		if other[reason] {
			return true
		}
	}
	return false
}

func reasonKnown(reason Reason) bool {
	for _, known := range Reasons {
		if known == reason {
			return true
		}
	}
	return false
}
