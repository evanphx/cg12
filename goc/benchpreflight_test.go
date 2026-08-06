package goc_test

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The pre-flight: what the two timing suites ask about the machine before they
// spend minutes measuring it.
//
// # Why it exists
//
// Both suites already refuse a contaminated run. `make bench-perf` has the
// noise-growth ceiling in comparePerfBench and `make bench-crypto` has the
// precision ceiling in checkCryptoBenchInstrument, and both say the right thing
// -- that the box cannot support the tolerances the run is about to be judged
// against. They say it at the end. During the work that produced the perf suite
// that cost about eleven minutes per attempt, repeatedly; two jobs ended up
// waiting for a quiet window and one abandoned its measurement. The information
// was available in the first two seconds and was collected in the last.
//
// So this is the same question, asked first. It does not replace either ceiling:
// those see the run that was actually measured, including contamination that
// starts after minute two, and they stay exactly as they were. This one costs
// about a second and a half and catches the common case, which is a box that was
// already busy when someone typed make.
//
// # What it measures
//
// Contention on the core the run will be pinned to, because that is what the
// timing suites are exposed to: every timed run is `taskset -c N binary`, so
// sixty-three busy cores and a free core N cost this instrument far less than
// one busy core N. Three readings, from two independent sources, over one window
// of about a second and a half:
//
//   - The share of the core a calibration burst actually got. The burst is a
//     child process launched through the suite's own pin prefix, so it is pinned
//     the same way, by the same mechanism, to the same core. Its CPU time comes
//     from wait4's rusage and its wall time from the parent's clock, and the
//     ratio is the fraction of that core the run can expect to have. A second
//     process pinned to the same core takes this straight to about a half, which
//     is the case that has cost this tree the most time.
//
//   - What that core spent on somebody else, from /proc/stat's per-CPU counters
//     across the same window, minus the burst's own CPU time. It is an
//     independent reading of the same contention -- the kernel's accounting
//     rather than ours -- and it also sees a competitor that is not CPU-bound
//     enough to show up in the share.
//
//   - The burst-to-burst spread of the calibration's wall time. The other two
//     readings are means over the window and a competitor that runs for 200 ms
//     of it barely moves them; a run whose speed changes underneath it shows up
//     here. The burst is deliberately half register arithmetic and half a
//     pointer chase over 8 MiB, so this reading is sensitive to a busy memory
//     system as well as to a busy core.
//
// A measured burst rather than a load average because this tree prefers a
// measurement and because the load average answers a different question: it is
// the whole box over the last minute, and both of those are wrong here. A run
// pinned to a quiet core on a loaded box is fine, a run sharing its core with
// one spinner on an otherwise idle box is not, and a load average cannot tell
// those apart.
//
// # What it does not measure
//
// Memory bandwidth and last-level cache pressure from cores this run is not on.
// They are real -- the chase and gcpress rows are exposed to them -- and gating
// on them needs a throughput number compared against a per-machine reference,
// which is a second baseline with all of the first one's problems. The count of
// busy cores on the box is reported for context and deliberately not gated on,
// and the noise-growth ceiling at the end of the run remains the backstop for
// what this cannot see.
const (
	// benchPreflightOverride, when set to "off", skips the pre-flight. It is for
	// the case where somebody wants a number out of a busy box and knows what
	// that number is worth. Documented in the Makefile and in both baselines.
	benchPreflightOverride = "GOC_BENCH_PREFLIGHT"

	// benchPreflightBurstEnv carries the round count to the burst child, which is
	// this same test binary re-executed. Re-execution rather than a compiled
	// helper because the pre-flight has to cost seconds: `go build` of anything
	// at all is a large fraction of the budget, and the workload wants to be in
	// this file next to the thresholds it feeds.
	benchPreflightBurstEnv = "GOC_BENCH_PREFLIGHT_BURST"

	// benchPreflightBursts is how many measured bursts the spread is taken over,
	// and benchPreflightBurstTarget is what each one is sized to. Six times 200 ms
	// is a window long enough for /proc/stat's 10 ms accounting granularity to
	// resolve a tenth of a core, and short enough that the whole pre-flight is
	// under two seconds.
	benchPreflightBursts      = 6
	benchPreflightBurstTarget = 200 * time.Millisecond

	// benchPreflightShareFloor is the least of its core the burst may get.
	//
	// One competing CPU-bound process on the same core halves it. Measured idle
	// on this box the share is 0.97 to 0.99 -- the residue is process startup,
	// which is wall time that is not the burst's CPU time -- so the floor sits
	// well below what a quiet box produces and well above what a shared core
	// does.
	benchPreflightShareFloor = 0.90

	// benchPreflightForeignCeiling is how much of the core's time may go to work
	// that is not this run's. Idle, this reads 0.00 to 0.02 on this box; the
	// figure a shared core produces is whatever the competitor is taking, which
	// for one spinner is about a half.
	benchPreflightForeignCeiling = 0.10

	// benchPreflightSpreadCeiling is how far the slowest measured burst may sit
	// above the fastest.
	//
	// The loosest of the three, and deliberately: it is the reading that has a
	// process launch inside it, so it carries the variation of an exec as well as
	// the variation of the work. Idle on this box it is 1% to 4%. A competitor
	// arriving part way through the window puts it past 20%.
	benchPreflightSpreadCeiling = 0.12
)

// benchPreflightSuite is what the pre-flight needs to know about the caller: how
// it pins, what it costs if it is allowed to proceed, and what to name in the
// refusal.
type benchPreflightSuite struct {
	// target is the make target a person typed, and cost is how long it takes.
	target string
	cost   string
	// pin is the command prefix every timed run of this suite is launched
	// through, and pinNote is the human-readable form the suite logs.
	pin     []string
	pinNote string
	// coreVar is the environment variable that moves this suite to another core.
	coreVar string
}

// requireQuietBoxBefore refuses to start a timing run on a box that cannot
// support one.
//
// It is called before the binaries are built, not just before they are timed:
// the builds are a minute or two of their own and there is no reason to spend
// them on a run that is going to be thrown away.
func requireQuietBoxBefore(t *testing.T, suite benchPreflightSuite) {
	t.Helper()

	if strings.EqualFold(strings.TrimSpace(os.Getenv(benchPreflightOverride)), "off") {
		t.Logf("pre-flight skipped (%s=off). Nothing has checked whether this box can support a timing\n"+
			"measurement; if it cannot, %s will say so in %s at the noise ceiling.",
			benchPreflightOverride, suite.target, suite.cost)
		return
	}

	reading, err := measureBenchPreflight(suite)
	if err != nil {
		// A pre-flight that cannot run is not a reason to refuse to measure. The
		// end-of-run ceilings are still there, and this follows the same line the
		// pin functions take when taskset is missing: say what was not done, and
		// carry on.
		t.Logf("pre-flight could not measure this box (%v), so nothing has checked whether it is quiet.\n"+
			"The run will proceed and the noise ceiling at the end is what will catch a contaminated one.", err)
		return
	}

	var refusals []string
	if reading.share < benchPreflightShareFloor {
		refusals = append(refusals, fmt.Sprintf(
			"the calibration burst got %.1f%% of %s (floor %.0f%%) -- it is sharing that core with something",
			reading.share*100, reading.where, benchPreflightShareFloor*100))
	}
	if reading.foreign > benchPreflightForeignCeiling {
		refusals = append(refusals, fmt.Sprintf(
			"%s spent %.1f%% of the window on work that was not this run's (ceiling %.0f%%), per /proc/stat",
			reading.where, reading.foreign*100, benchPreflightForeignCeiling*100))
	}
	if reading.spread > benchPreflightSpreadCeiling {
		refusals = append(refusals, fmt.Sprintf(
			"the burst's wall time spread %.1f%% across %d repetitions (ceiling %.0f%%) -- the speed of this core is changing underneath it",
			reading.spread*100, benchPreflightBursts, benchPreflightSpreadCeiling*100))
	}

	require.Empty(t, refusals, benchPreflightRefusal(suite, reading, refusals))

	t.Logf("pre-flight: %s is quiet enough to measure on. In %.1fs, the calibration burst got %.1f%% of it,\n"+
		"%.1f%% of its time went to something else, and the burst's wall time spread %.1f%% across %d\n"+
		"repetitions. Box-wide in the same window: %s busy, which is context and not gated on.",
		reading.where, reading.elapsed.Seconds(), reading.share*100, reading.foreign*100,
		reading.spread*100, benchPreflightBursts, reading.busyCores)
}

// benchPreflightRefusal is the message. It is a function rather than a constant
// for the reason perfBenchSlowerMessage is: it is the whole product of this
// check, and the thing it has to do is convince a reader in one screen that the
// compiler is not implicated and that there are three specific things they can
// do next.
func benchPreflightRefusal(suite benchPreflightSuite, reading benchPreflightReading, refusals []string) string {
	return fmt.Sprintf(
		"this box cannot support a trustworthy timing measurement, so %s is refusing to start rather\n"+
			"than taking %s to reach the same conclusion at the end. This is a statement about the\n"+
			"machine and not about the compiler: nothing here says anything about goc, and no baseline in\n"+
			"this directory has been consulted yet.\n\n"+
			"What the pre-flight measured, in %.1f seconds on %s, %s:\n"+
			"  %s\n"+
			"  (box-wide in the same window, %s busy -- not gated on, because this run is pinned and a\n"+
			"  busy core it is not on costs it far less than the one it is)\n\n"+
			"Both timing baselines were cut on an idle box and every tolerance in them is that box's\n"+
			"noise. A run on this one would either fail for a reason no diff explains or pass a tolerance\n"+
			"it did not earn, and both cost more than the wait does.\n\n"+
			"Three things to do, in the order they are usually right:\n\n"+
			"  1. Wait for the box to go quiet. `uptime` for the trend, and for what is on this core:\n\n"+
			"         top -H -1                 # the per-CPU rows say which core, `1` toggles them\n"+
			"         ps -eo pid,psr,pcpu,comm --sort=-pcpu | head -20\n\n"+
			"  2. Move this run to a core nothing else is using. %s names it, and the two timing suites\n"+
			"     deliberately pick different cores so they can run at once -- bench-perf takes the second\n"+
			"     highest it is allowed and bench-crypto the highest, so a third timing job needs a third\n"+
			"     core named explicitly:\n\n"+
			"         %s=N %s\n\n"+
			"  3. Measure anyway, if you want a number from a busy box and know what it is worth:\n\n"+
			"         %s=off %s\n\n"+
			"     which is what this suite did before this check existed: it measures for %s and then, if\n"+
			"     the contamination was large enough to show, refuses at the noise ceiling with the run's\n"+
			"     own numbers in hand. That is a fine thing to do deliberately and an expensive thing to\n"+
			"     do by accident.",
		suite.target, suite.cost,
		reading.elapsed.Seconds(), reading.where, suite.pinNote,
		strings.Join(refusals, "\n  "), reading.busyCores,
		suite.coreVar, suite.coreVar, suite.target,
		benchPreflightOverride, suite.target,
		suite.cost)
}

// benchPreflightReading is what one pre-flight measured.
type benchPreflightReading struct {
	// where names what the readings are about: one core, or the set this process
	// is allowed on when the suite is not pinning.
	where string
	// share is the burst's CPU time over its wall time: the fraction of the core
	// it actually got.
	share float64
	// foreign is the fraction of the window the core spent on work that was not
	// the burst's, from /proc/stat.
	foreign float64
	// spread is (slowest - fastest) / fastest over the measured bursts.
	spread float64
	// busyCores is the whole-box context line, not gated on.
	busyCores string
	elapsed   time.Duration
}

// measureBenchPreflight runs the calibration and reads the kernel's counters
// around it.
func measureBenchPreflight(suite benchPreflightSuite) (benchPreflightReading, error) {
	overall := time.Now()
	self, err := os.Executable()
	if err != nil {
		return benchPreflightReading{}, fmt.Errorf("could not find this test binary to launch the calibration burst: %w", err)
	}

	cores, where, err := benchPreflightCores(suite)
	if err != nil {
		return benchPreflightReading{}, err
	}

	// Two short probes to size the burst, because the fixed number of rounds that
	// takes 200 ms is a property of the machine. Two points and not one: the wall
	// time of a burst is a process launch plus the work, and only the second term
	// scales, so a single point sized on this box would size wrong on a slower or
	// faster one.
	small, err := runBenchPreflightBurst(self, suite.pin, 2)
	if err != nil {
		return benchPreflightReading{}, err
	}
	large, err := runBenchPreflightBurst(self, suite.pin, 10)
	if err != nil {
		return benchPreflightReading{}, err
	}
	rounds := benchPreflightRounds(small.wall, large.wall)

	started := time.Now()
	before, err := readProcStat()
	if err != nil {
		return benchPreflightReading{}, err
	}
	var burstCPU, fastest, slowest time.Duration
	for index := 0; index < benchPreflightBursts; index++ {
		burst, err := runBenchPreflightBurst(self, suite.pin, rounds)
		if err != nil {
			return benchPreflightReading{}, err
		}
		burstCPU += burst.cpu
		if index == 0 || burst.wall < fastest {
			fastest = burst.wall
		}
		if burst.wall > slowest {
			slowest = burst.wall
		}
	}
	after, err := readProcStat()
	if err != nil {
		return benchPreflightReading{}, err
	}
	window := time.Since(started)

	// The share is taken over the same window the counters cover, so the two
	// readings are of one thing and can be compared with each other. It is a
	// fraction of one core and not of the set: the burst is one process at
	// GOMAXPROCS=1, so what it can get is one core whether or not it is pinned.
	share := burstCPU.Seconds() / window.Seconds()
	busy, err := busyFraction(before, after, cores)
	if err != nil {
		return benchPreflightReading{}, err
	}
	// Our own CPU time is spread over the cores the run may use; when the suite
	// pins, that is one core and the two fractions are directly comparable.
	ours := burstCPU.Seconds() / (window.Seconds() * float64(len(cores)))
	foreign := busy - ours
	if foreign < 0 {
		foreign = 0
	}

	return benchPreflightReading{
		where:     where,
		share:     share,
		foreign:   foreign,
		spread:    float64(slowest-fastest) / float64(fastest),
		busyCores: busyCoreNote(before, after),
		elapsed:   time.Since(overall),
	}, nil
}

// benchPreflightRounds solves the two probes for the round count that lands a
// burst on benchPreflightBurstTarget, and clamps it: a machine slow enough or a
// probe noisy enough to ask for a burst of many seconds has defeated the point
// of a pre-flight that costs seconds.
func benchPreflightRounds(small, large time.Duration) int {
	perRound := float64(large-small) / 8
	if perRound <= 0 {
		return 8
	}
	launch := float64(small) - 2*perRound
	rounds := int((float64(benchPreflightBurstTarget) - launch) / perRound)
	if rounds < 1 {
		return 1
	}
	if maximum := int(float64(2*benchPreflightBurstTarget) / perRound); rounds > maximum {
		return maximum
	}
	return rounds
}

// benchPreflightCores says which cores the readings are about, and how to name
// them in a message.
//
// The cores come from the pin prefix the suite is actually going to use, not
// from a second guess at which core that is: if the suite pins to core 62 and
// this read core 63, the pre-flight would be measuring a core nothing is about
// to run on.
func benchPreflightCores(suite benchPreflightSuite) ([]int, string, error) {
	if len(suite.pin) > 0 {
		cores, err := parseCPUList(suite.pin[len(suite.pin)-1])
		if err != nil {
			return nil, "", fmt.Errorf("could not tell which core %q pins to: %w", strings.Join(suite.pin, " "), err)
		}
		if len(cores) == 1 {
			return cores, fmt.Sprintf("core %d", cores[0]), nil
		}
		return cores, fmt.Sprintf("the %d cores this run pins to", len(cores)), nil
	}
	cores, err := allowedCPUs()
	if err != nil {
		return nil, "", err
	}
	return cores, fmt.Sprintf("the %d cores this process is allowed on (this run is not pinned)", len(cores)), nil
}

// benchPreflightBurstResult is one launch of the calibration burst.
type benchPreflightBurstResult struct {
	// wall is what the parent's clock saw, including the launch.
	wall time.Duration
	// cpu is user plus system time from wait4's rusage: what the child actually
	// got off the core, which is the half of the ratio a competitor moves.
	cpu time.Duration
}

// runBenchPreflightBurst launches the burst through the suite's own pin prefix
// and times it from outside.
//
// GOMAXPROCS=1 and GOGC=off in the child so that its CPU time is the burst's and
// not a collector's, and so that a child pinned to one core is not also fighting
// its own runtime for it.
func runBenchPreflightBurst(self string, pin []string, rounds int) (benchPreflightBurstResult, error) {
	argv := append(append([]string{}, pin...), self,
		"-test.run=^TestBenchPreflightCalibrationBurst$", "-test.count=1")
	burst := exec.Command(argv[0], argv[1:]...)
	burst.Env = append(os.Environ(),
		benchPreflightBurstEnv+"="+strconv.Itoa(rounds),
		"GOMAXPROCS=1",
		"GOGC=off")

	started := time.Now()
	output, err := burst.CombinedOutput()
	wall := time.Since(started)
	if err != nil {
		return benchPreflightBurstResult{}, fmt.Errorf("the calibration burst would not run (%w):\n%s", err, output)
	}
	state := burst.ProcessState
	return benchPreflightBurstResult{wall: wall, cpu: state.UserTime() + state.SystemTime()}, nil
}

// TestBenchPreflightCalibrationBurst is the pre-flight's workload, run as a
// child of the pre-flight through the same taskset prefix the suite times its
// binaries with. It skips unless the pre-flight asked for it, so an ordinary run
// of this package never executes it.
func TestBenchPreflightCalibrationBurst(t *testing.T) {
	requested := os.Getenv(benchPreflightBurstEnv)
	if requested == "" {
		t.Skipf("%s is not set; this is the timing pre-flight's calibration burst, not a test of the compiler",
			benchPreflightBurstEnv)
	}
	rounds, err := strconv.Atoi(requested)
	require.NoError(t, err, "%s must be a round count", benchPreflightBurstEnv)

	// Printed, so that neither compiler nor linker can decide the burst had no
	// effect and delete it.
	fmt.Fprintf(os.Stderr, "burst %d\n", benchPreflightBurst(rounds))
}

// benchPreflightBurstChaseSlots is the size of the pointer chase, in 8-byte
// slots: 8 MiB, which is past any per-core cache on this box and into the part
// of the memory system that is shared.
const benchPreflightBurstChaseSlots = 1 << 20

// benchPreflightBurst is the calibration work: half register arithmetic, half a
// dependent chase through 8 MiB.
//
// Two halves because the two are exposed to different contention. The arithmetic
// half measures the core, and it is what halves when something else is pinned to
// the same one. The chase half leaves the core and is what moves when the memory
// system is busy -- which is the contamination a per-core reading cannot see, and
// which reaches this pre-flight only through the burst-to-burst spread.
//
// The ring is built once per process and the rounds run over it, so that the
// round count scales the work and the setup is a constant the two probes solve
// out.
func benchPreflightBurst(rounds int) uint64 {
	ring := make([]uint32, benchPreflightBurstChaseSlots)
	for index := range ring {
		ring[index] = uint32(index)
	}
	// A fixed permutation, from a fixed generator: the same chase every time this
	// runs, on every machine, so that two bursts differ only in what the machine
	// was doing.
	state := uint64(0x9E3779B97F4A7C15)
	for index := len(ring) - 1; index > 0; index-- {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		swap := int(state % uint64(index+1))
		ring[index], ring[swap] = ring[swap], ring[index]
	}

	accumulator := uint64(1)
	cursor := uint32(0)
	for round := 0; round < rounds; round++ {
		for step := 0; step < 200000; step++ {
			accumulator = accumulator*6364136223846793005 + 1442695040888963407
			accumulator ^= accumulator >> 29
		}
		for step := 0; step < 100000; step++ {
			cursor = ring[cursor]
			accumulator += uint64(cursor)
		}
	}
	return accumulator
}

// procStat is one sample of /proc/stat's per-CPU counters, in the kernel's ticks.
type procStat struct {
	busy  map[int]uint64
	total map[int]uint64
}

// readProcStat samples the per-CPU counters.
//
// busy is everything that is not idle and not iowait, which includes steal: time
// a hypervisor took is time this run did not get, and for the question being
// asked here that is the same thing as a neighbour taking it.
func readProcStat() (procStat, error) {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return procStat{}, fmt.Errorf("could not read /proc/stat, so this box's per-core occupancy is unknown: %w", err)
	}
	sample := procStat{busy: map[int]uint64{}, total: map[int]uint64{}}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}
		core, err := strconv.Atoi(strings.TrimPrefix(fields[0], "cpu"))
		if err != nil {
			// The aggregate "cpu" line, which is the sum of the others.
			continue
		}
		var total, idle uint64
		for index, field := range fields[1:] {
			ticks, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return procStat{}, fmt.Errorf("could not parse /proc/stat line %q", line)
			}
			total += ticks
			// Fields 4 and 5 of a cpu line are idle and iowait.
			if index == 3 || index == 4 {
				idle += ticks
			}
		}
		sample.busy[core] = total - idle
		sample.total[core] = total
	}
	if len(sample.busy) == 0 {
		return procStat{}, fmt.Errorf("/proc/stat had no per-CPU lines")
	}
	return sample, nil
}

// busyFraction is the share of the given cores' time that went to something,
// across the window between two samples.
//
// A fraction of ticks over ticks and not a conversion to seconds, so that the
// kernel's tick rate never enters the arithmetic.
func busyFraction(before, after procStat, cores []int) (float64, error) {
	var busy, total uint64
	for _, core := range cores {
		beforeTotal, known := before.total[core]
		if !known {
			return 0, fmt.Errorf("/proc/stat has no counters for core %d, which is the core this run would use", core)
		}
		busy += after.busy[core] - before.busy[core]
		total += after.total[core] - beforeTotal
	}
	if total == 0 {
		return 0, fmt.Errorf("/proc/stat's counters for %d core(s) did not advance across the pre-flight window", len(cores))
	}
	return float64(busy) / float64(total), nil
}

// busyCoreNote counts the cores that were more than half busy across the window.
// Context for the log line and for the refusal; nothing is gated on it, because
// this suite pins and a busy core it is not on costs it much less than the core
// it is on.
func busyCoreNote(before, after procStat) string {
	busy := 0
	for core, beforeBusy := range before.busy {
		total := after.total[core] - before.total[core]
		if total == 0 {
			continue
		}
		if float64(after.busy[core]-beforeBusy)/float64(total) > 0.5 {
			busy++
		}
	}
	return fmt.Sprintf("%d of %d cores", busy, len(before.busy))
}

// parseCPUList parses the CPU list syntax the kernel prints in
// Cpus_allowed_list and taskset accepts on the command line: comma-separated
// cores and inclusive ranges, "3", "3,5", "0-7,16-23".
func parseCPUList(list string) ([]int, error) {
	trimmed := strings.TrimSpace(list)
	var cores []int
	for _, span := range strings.Split(trimmed, ",") {
		first, last, ranged := strings.Cut(span, "-")
		if !ranged {
			last = first
		}
		low, lowErr := strconv.Atoi(strings.TrimSpace(first))
		high, highErr := strconv.Atoi(strings.TrimSpace(last))
		if lowErr != nil || highErr != nil || high < low {
			return nil, fmt.Errorf("could not parse the CPU list %q", trimmed)
		}
		for core := low; core <= high; core++ {
			cores = append(cores, core)
		}
	}
	if len(cores) == 0 {
		return nil, fmt.Errorf("the CPU list %q named no core", trimmed)
	}
	sort.Ints(cores)
	return cores, nil
}
