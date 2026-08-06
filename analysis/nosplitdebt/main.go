package main

// nosplitdebt regenerates arm64.noSplitDebt -- the register of nosplit chains
// that were already over the reserve when the frame budget was introduced.
//
// The register is a floor: a chain it names may be as deep as it records and no
// deeper, and a chain it does not name may not exceed the reserve at all. That
// makes the set of configurations it is generated from part of its meaning. A
// chain the generation never saw is a chain the budget will reject the first
// time somebody compiles the program that contains it, which is what happened to
// goc/testdata/stdlib_os_exec_echo.go: the original recipe drove seven runtime
// pack roots and four whole programs, none of which reaches os/exec's fork path,
// so syscall.runtime_AfterForkInChild's 976-byte chain was a 51st entry the
// recipe could not have found.
//
// The blind spot was not the number of configurations, it was their shape. Two
// properties decide whether a chain is visible at all:
//
//   - Which functions are in the module. The budget is a per-module walk, so a
//     chain is only measured where every one of its frames is compiled into the
//     same module. Four whole programs sample four points of a 406-program
//     corpus; a chain rooted in a package none of the four imports is invisible
//     no matter how many optimization levels those four are built at.
//   - Where the module boundary falls. `goc build-runtime` compiles the runtime
//     alone, so a chain whose upper frames live above the runtime -- as
//     syscall.runtime_AfterForkInChild and runtime.clearSignalHandlers do -- is
//     cut off at the boundary and measures short. The seven pack roots are all
//     of this shape, so fourteen of the original twenty-two configurations
//     structurally could not see the chain, and the remaining eight were the
//     four-program sample.
//
// So this driver sweeps the product of both: every program the corpus contains,
// built whole-program (one module, every frame visible) and pack-split (the
// module boundary the capability matrix and the pack cache actually use), each
// with and without -O, plus `build-runtime` for each capability-matrix pack root
// with and without -O. Every configuration the tree can be compiled in, in other
// words, rather than a sample of them.
//
// Heights are read from the shipping compiler with GOC_DEBUG_NOSPLIT=heights and
// the real 920-byte limit, which is deliberate: the nosplit inliner sizes its
// allowance from the same limit, so a run with GOC_NOSPLIT_LIMIT raised would
// inline into nosplit callers far past what the shipping compiler does and
// measure frames no build produces. There is no circularity in running the
// shipping compiler with the register in place -- opt.InlineIntoNoSplitCallers
// is handed a budget built with no Recorded field at all
// (arm64/nosplit_measure.go), so the register cannot change a single frame. A
// configuration the budget rejects still prints its heights before it fails,
// which is what lets the recipe find the entry that would fix it.
//
// This is measurement, not a shipped tool.

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// runtimeCapabilityPackRoots mirrors cmd/goc's list of capability-matrix pack
// roots. It is duplicated rather than imported because that list lives in a test
// file, and a driver that only builds when the test package does is a driver
// nobody runs.
var runtimeCapabilityPackRoots = []string{
	"",
	"net/http",
	"net/smtp",
	"crypto/x509",
	"crypto/ecdsa",
	"crypto/ecdh",
	"crypto/hpke",
}

// originalWholePrograms is the four-program whole-program arm of the recipe that
// produced the committed register. It is kept so this driver can report what the
// widened recipe finds that the original could not, which is the number that
// says whether the blind spot was one entry or twenty.
var originalWholePrograms = []string{
	"runtime_lock_osthread.go",
	"runtime_gc_concurrent_mark.go",
	"stdlib_http_tls_client_server.go",
	"stdlib_compress_zlib_lzw.go",
}

// arm names one of the three module shapes the sweep drives.
type arm string

const (
	armPack  arm = "pack"  // goc build-runtime: the runtime as a module of its own
	armWhole arm = "whole" // goc prog.go: every frame in one module
	armSplit arm = "split" // goc -runtime pack prog.go: the program above a prebuilt runtime
)

// configuration is one build the sweep performs.
type configuration struct {
	arm      arm
	label    string
	optimize bool
	// original reports whether this configuration was one of the twenty-two the
	// committed register was generated from.
	original bool
	run      func(work string) (stderr string, err error)
}

// outcome is what one configuration produced.
type outcome struct {
	config configuration
	// heights is every nosplit chain over the reserve this configuration
	// reported, by function name.
	heights map[string]int
	// rejected reports that the build failed on the frame budget itself. Its
	// heights are still valid -- they are printed from the finished walk, before
	// the error is returned -- and are the input that fixes the register.
	rejected bool
	// broken reports a build that failed for some other reason and therefore
	// measured nothing.
	broken  bool
	message string
	seconds float64
}

var heightLine = regexp.MustCompile(`^goc: nosplit height:\s+(\d+)\s+(\S+)$`)

func main() {
	compiler := flag.String("goc", "", "path to a built goc binary")
	work := flag.String("work", "", "scratch directory")
	workers := flag.Int("j", 8, "concurrent compiles")
	register := flag.String("register", "arm64/nosplit_debt.go", "the register to compare against")
	update := flag.Bool("update", false, "rewrite the register's map from the sweep")
	arms := flag.String("arms", "pack,whole,split", "which module shapes to drive")
	verbose := flag.Bool("v", false, "print every configuration as it finishes")
	flag.Parse()
	programs := flag.Args()
	if *compiler == "" || *work == "" || len(programs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: nosplitdebt -goc goc -work dir [-j n] [-register file] [-update] program.go...")
		os.Exit(2)
	}
	enabled := map[arm]bool{}
	for _, name := range strings.Split(*arms, ",") {
		enabled[arm(strings.TrimSpace(name))] = true
	}

	absoluteCompiler, err := filepath.Abs(*compiler)
	if err != nil {
		fail(err)
	}
	if err := os.MkdirAll(*work, 0o755); err != nil {
		fail(err)
	}

	// The pack arm runs first and alone: the split arm needs the packs it
	// writes, and building fourteen packs concurrently with eight hundred
	// program compiles only makes the packs late.
	packs := map[bool]string{}
	var outcomes []outcome
	if enabled[armPack] || enabled[armSplit] {
		packConfigurations, packPaths := packSweep(absoluteCompiler, *work)
		packs = packPaths
		results := run(packConfigurations, *work, *workers, *verbose)
		if enabled[armPack] {
			outcomes = append(outcomes, results...)
		}
		for _, result := range results {
			if result.broken {
				fmt.Fprintf(os.Stderr, "nosplitdebt: pack %s failed to build: %s\n", result.config.label, result.message)
			}
		}
	}

	var programConfigurations []configuration
	for _, program := range programs {
		for _, optimize := range []bool{false, true} {
			if enabled[armWhole] {
				programConfigurations = append(programConfigurations, wholeConfiguration(absoluteCompiler, program, optimize))
			}
			if enabled[armSplit] && packs[optimize] != "" {
				programConfigurations = append(programConfigurations, splitConfiguration(absoluteCompiler, program, optimize, packs[optimize]))
			}
		}
	}
	outcomes = append(outcomes, run(programConfigurations, *work, *workers, *verbose)...)

	report(outcomes, *register, *update)
}

// packSweep is `goc build-runtime` for every capability-matrix pack root, with
// and without -O. It returns the configurations and, for each optimization
// level, the comma-separated pack list the split arm links against -- the same
// list and the same precedence the capability matrix uses.
func packSweep(compiler, work string) ([]configuration, map[bool]string) {
	var configurations []configuration
	built := map[bool][]string{}
	for _, optimize := range []bool{false, true} {
		for index, root := range runtimeCapabilityPackRoots {
			output := filepath.Join(work, fmt.Sprintf("pack%d%s.gocrt", index, suffix(optimize)))
			built[optimize] = append(built[optimize], output)
			label := root
			if label == "" {
				label = "runtime-only"
			}
			arguments := []string{"build-runtime", "-o", output, "-packages", root}
			if optimize {
				arguments = append(arguments, "-O")
			}
			configurations = append(configurations, configuration{
				arm:      armPack,
				label:    fmt.Sprintf("pack %s%s", label, suffix(optimize)),
				optimize: optimize,
				original: true,
				run: func(string) (string, error) {
					// CG12_NOCACHE keeps a pack from being served out of the
					// on-disk cache, which would measure a compiler that is not
					// this one.
					return compile(compiler, arguments, "CG12_NOCACHE=1")
				},
			})
		}
	}
	packs := map[bool]string{}
	for optimize, list := range built {
		packs[optimize] = strings.Join(list, ",")
	}
	return configurations, packs
}

// wholeConfiguration compiles one program with no prebuilt runtime, so the
// runtime, the standard library and the program are one module and every frame
// of every chain is visible to the walk at once. This is the arm that sees a
// chain like syscall.runtime_AfterForkInChild's, whose upper frames live above
// the runtime.
//
// A bare `goc program.go` no longer means that. Since the pack cache became the
// default (cmd/goc/autopack.go), an ordinary compile finds or builds the pack its
// import list needs and links against it, which is the split arm's module
// boundary arriving without being asked for. The two switches below are what
// keeps this arm whole, and both are named on purpose:
//
//   - GOC_AUTOPACK=0 is the switch whose meaning is "do not choose a pack", and
//     it is the one that makes the module whole. Without it this arm silently
//     measures split frames and the register is regenerated from a strictly
//     weaker view than it documents itself as having.
//   - CG12_NOCACHE=1 is what the pack arm three functions above sets, for the
//     same reason it sets it: nothing in this measurement may be served out of a
//     cache that some other compiler filled, or the heights are not this
//     compiler's.
func wholeConfiguration(compiler, program string, optimize bool) configuration {
	arguments := []string{"-o", "/dev/null", program}
	if optimize {
		arguments = append([]string{"-O"}, arguments...)
	}
	return configuration{
		arm:      armWhole,
		label:    fmt.Sprintf("whole %s%s", filepath.Base(program), suffix(optimize)),
		optimize: optimize,
		original: containsName(originalWholePrograms, filepath.Base(program)),
		run: func(string) (string, error) {
			return compile(compiler, arguments, "GOC_AUTOPACK=0", "CG12_NOCACHE=1")
		},
	}
}

// splitConfiguration compiles one program against the prebuilt runtime packs,
// which is the module boundary the capability matrix, `goc compile-batch` and
// the pack cache all use. The walk then stops at the boundary, so this arm
// measures less of any chain that crosses it -- but the frames it does measure
// are the frames this mode produces, and they are not the whole-program ones:
// the inliner's headroom is computed per module, so a nosplit function in the
// program module is offered a different allowance here.
func splitConfiguration(compiler, program string, optimize bool, packs string) configuration {
	arguments := []string{"-runtime", packs, "-o", "/dev/null", program}
	if optimize {
		arguments = append([]string{"-O"}, arguments...)
	}
	return configuration{
		arm:      armSplit,
		label:    fmt.Sprintf("split %s%s", filepath.Base(program), suffix(optimize)),
		optimize: optimize,
		original: false,
		run: func(string) (string, error) {
			return compile(compiler, arguments)
		},
	}
}

// compile runs one build with the heights dump turned on and returns its stderr.
//
// The limit is left alone. GOC_NOSPLIT_LIMIT would stop the budget rejecting
// anything, which is tempting for a measurement run, but it is also the number
// opt.InlineIntoNoSplitCallers sizes its allowance from: raising it makes the
// inliner spend stack no shipping build spends, and the heights that come back
// are then nobody's frames.
func compile(compiler string, arguments []string, environment ...string) (string, error) {
	build := exec.Command(compiler, arguments...)
	build.Env = append(os.Environ(), "GOC_DEBUG_NOSPLIT=heights")
	build.Env = append(build.Env, environment...)
	var stderr strings.Builder
	build.Stderr = &stderr
	build.Stdout = nil
	err := build.Run()
	return stderr.String(), err
}

// run drives the configurations across workers and returns what each measured.
func run(configurations []configuration, work string, workers int, verbose bool) []outcome {
	outcomes := make([]outcome, len(configurations))
	queue := make(chan int)
	var running sync.WaitGroup
	var progress sync.Mutex
	done := 0
	started := time.Now()
	for worker := 0; worker < workers; worker++ {
		running.Add(1)
		go func() {
			defer running.Done()
			for index := range queue {
				config := configurations[index]
				begin := time.Now()
				stderr, err := config.run(work)
				result := outcome{
					config:  config,
					heights: parseHeights(stderr),
					seconds: time.Since(begin).Seconds(),
				}
				if err != nil {
					if strings.Contains(stderr, "nosplit frame budget") {
						result.rejected = true
					} else {
						result.broken = true
					}
					result.message = firstLine(stderr, err)
				}
				outcomes[index] = result
				progress.Lock()
				done++
				if verbose || done%100 == 0 {
					fmt.Fprintf(os.Stderr, "nosplitdebt: %d/%d %s (%.1fs)\n",
						done, len(configurations), config.label, result.seconds)
				}
				progress.Unlock()
			}
		}()
	}
	for index := range configurations {
		queue <- index
	}
	close(queue)
	running.Wait()
	fmt.Fprintf(os.Stderr, "nosplitdebt: %d configurations in %.1fs\n", len(configurations), time.Since(started).Seconds())
	return outcomes
}

func parseHeights(stderr string) map[string]int {
	heights := map[string]int{}
	scanner := bufio.NewScanner(strings.NewReader(stderr))
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for scanner.Scan() {
		match := heightLine.FindStringSubmatch(scanner.Text())
		if match == nil {
			continue
		}
		height, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		if height > heights[match[2]] {
			heights[match[2]] = height
		}
	}
	return heights
}

// maximum folds the outcomes matching keep into one register: the greatest
// height each function was measured at, and the configuration that measured it.
func maximum(outcomes []outcome, keep func(outcome) bool) (map[string]int, map[string]string) {
	heights := map[string]int{}
	source := map[string]string{}
	for _, result := range outcomes {
		if result.broken || !keep(result) {
			continue
		}
		for name, height := range result.heights {
			if height > heights[name] {
				heights[name] = height
				source[name] = result.config.label
			}
		}
	}
	return heights, source
}

func report(outcomes []outcome, register string, update bool) {
	perArm := map[arm]int{}
	rejected, broken := 0, 0
	for _, result := range outcomes {
		perArm[result.config.arm]++
		if result.rejected {
			rejected++
		}
		if result.broken {
			broken++
		}
	}
	fmt.Printf("configurations: %d total", len(outcomes))
	for _, name := range []arm{armPack, armWhole, armSplit} {
		if perArm[name] > 0 {
			fmt.Printf("  %s=%d", name, perArm[name])
		}
	}
	fmt.Printf("\n")
	fmt.Printf("outcomes: %d measured, %d rejected by the budget (heights still valid), %d failed to compile\n",
		len(outcomes)-broken, rejected, broken)
	for _, result := range outcomes {
		if result.broken {
			fmt.Printf("  FAILED  %-56s %s\n", result.config.label, result.message)
		}
	}
	for _, result := range outcomes {
		if result.rejected {
			fmt.Printf("  BUDGET  %-56s %s\n", result.config.label, result.message)
		}
	}

	widened, source := maximum(outcomes, func(outcome) bool { return true })
	original, _ := maximum(outcomes, func(result outcome) bool { return result.config.original })
	committed, err := readRegister(register)
	if err != nil {
		fail(err)
	}

	fmt.Printf("\nregister: committed=%d original-recipe=%d widened-recipe=%d\n",
		len(committed), len(original), len(widened))

	describe("the widened recipe finds, the original recipe does not", widened, original, source)
	describe("committed, and the widened recipe does not reach", committed, widened, source)
	fmt.Printf("\nagainst the committed register:\n")
	changes := 0
	for _, name := range sortedByHeight(widened) {
		height := widened[name]
		recorded, ok := committed[name]
		switch {
		case !ok:
			fmt.Printf("  ADDED   %-52s %5d   first seen: %s\n", name, height, source[name])
			changes++
		case height > recorded:
			fmt.Printf("  RAISED  %-52s %5d (was %d)   deepest at: %s\n", name, height, recorded, source[name])
			changes++
		case height < recorded:
			fmt.Printf("  LOWERED %-52s %5d (was %d)\n", name, height, recorded)
			changes++
		}
	}
	for _, name := range sortedByHeight(committed) {
		if _, ok := widened[name]; !ok {
			fmt.Printf("  REMOVED %-52s (was %d)\n", name, committed[name])
			changes++
		}
	}
	if changes == 0 {
		fmt.Printf("  no change: %d entries, same names, same heights\n", len(widened))
	}

	if !update {
		return
	}
	if err := writeRegister(register, widened); err != nil {
		fail(err)
	}
	fmt.Printf("\nwrote %s (%d entries)\n", register, len(widened))
}

// describe prints the entries in left that right does not reach, which is how
// the sweep says what widening the recipe bought.
func describe(title string, left, right map[string]int, source map[string]string) {
	var lines []string
	for _, name := range sortedByHeight(left) {
		if right[name] >= left[name] {
			continue
		}
		if _, ok := right[name]; ok {
			lines = append(lines, fmt.Sprintf("  %-52s %5d (other recipe: %d)   %s", name, left[name], right[name], source[name]))
			continue
		}
		lines = append(lines, fmt.Sprintf("  %-52s %5d (other recipe: never seen)   %s", name, left[name], source[name]))
	}
	fmt.Printf("\n%s: %d\n", title, len(lines))
	for _, line := range lines {
		fmt.Println(line)
	}
}

var registerBody = regexp.MustCompile(`(?s)(var noSplitDebt = map\[string\]int\{\n).*?(\n\}\n)`)

func readRegister(path string) (map[string]int, error) {
	source, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	match := registerBody.FindSubmatch(source)
	if match == nil {
		return nil, fmt.Errorf("%s: no `var noSplitDebt = map[string]int{` body", path)
	}
	entry := regexp.MustCompile(`"([^"]+)":\s*(\d+),`)
	heights := map[string]int{}
	for _, found := range entry.FindAllStringSubmatch(string(match[0]), -1) {
		height, err := strconv.Atoi(found[2])
		if err != nil {
			return nil, err
		}
		heights[found[1]] = height
	}
	return heights, nil
}

// writeRegister replaces the map body in place, so the file's documentation --
// which is most of what the register is -- survives regeneration.
func writeRegister(path string, heights map[string]int) error {
	source, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var body strings.Builder
	for _, name := range sortedByHeight(heights) {
		fmt.Fprintf(&body, "\t%q: %d,", name, heights[name])
		body.WriteString("\n")
	}
	replaced := registerBody.ReplaceAll(source, []byte("${1}"+strings.TrimSuffix(body.String(), "\n")+"${2}"))
	if err := os.WriteFile(path, replaced, 0o644); err != nil {
		return err
	}
	format := exec.Command("gofmt", "-w", path)
	format.Stderr = os.Stderr
	return format.Run()
}

func sortedByHeight(heights map[string]int) []string {
	names := make([]string, 0, len(heights))
	for name := range heights {
		names = append(names, name)
	}
	sort.Slice(names, func(left, right int) bool {
		if heights[names[left]] != heights[names[right]] {
			return heights[names[left]] > heights[names[right]]
		}
		return names[left] < names[right]
	})
	return names
}

func containsName(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}

func suffix(optimize bool) string {
	if optimize {
		return " -O"
	}
	return ""
}

func firstLine(stderr string, err error) string {
	for _, line := range strings.Split(stderr, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "goc: nosplit") {
			continue
		}
		return trimmed
	}
	return err.Error()
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "nosplitdebt: %v\n", err)
	os.Exit(1)
}
