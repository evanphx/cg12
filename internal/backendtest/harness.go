package backendtest

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanphx/cg12/internal/testenv"
)

// Harness turns objects a backend produced into a running process.
//
// It is the build half of the picture [Machine] starts: a machine says what can
// execute an image, a harness says how the image is put together. The two are
// separate because one machine has several build recipes -- the amd64 suite links
// freestanding against a hand-written _start so its images need nothing but the
// exit syscall, while the arm64 suite links a C driver against libc -- and all of
// them run the same way once built.
//
// A harness holds no state and resolves no tools until it is used, so declaring
// one at package scope in a test file costs nothing and skips nothing.
type Harness struct {
	// Machine is the architecture the images target.
	Machine Machine

	// Compiler names the C compiler / assembler driver that turns source inputs
	// into objects, most preferred first. They are alternatives for one job (a
	// cross compiler or the native one), so the first one present is used.
	Compiler []string

	// CompileArgs are passed to the compiler ahead of -c, typically the flag
	// selecting the target ("--target=x86_64-linux-gnu").
	CompileArgs []string

	// Linker names the linker, most preferred first, with the same
	// alternatives-for-one-job meaning as Compiler. A C compiler driver is a fine
	// choice here: it links, and it brings libc with it.
	Linker []string

	// LinkArgs are passed to the linker ahead of -o, such as -static -nostdlib
	// for a freestanding image or -lpthread for a driver that needs it.
	LinkArgs []string

	// StartStub is assembly defining the image's entry point, linked in ahead of
	// everything else. Leave it empty when the entry point comes from elsewhere
	// -- a libc crt0 reaching a C driver's main, say.
	StartStub string
}

// Input is one file linked into the image.
//
// A backend test's own output arrives as object bytes ([Object]); the scaffolding
// around it -- an entry stub, a C driver calling the generated function -- arrives
// as source the harness compiles first ([AsmSource], [CSource]).
type Input struct {
	// Name is the file's base name inside the harness's temporary directory. Its
	// extension decides what happens to Data: ".o" is linked as it stands,
	// anything else is compiled first.
	Name string

	// Data is the file's contents.
	Data []byte

	// Diagnose, when set, describes this input for the failure message if
	// compiling or linking fails -- a disassembly of the object, most usefully,
	// since a link error against generated code is nearly unreadable without one.
	// It is called only on failure, so it may be expensive.
	Diagnose func() string
}

// Object is a compiled object to link as it stands, the usual output of a
// backend under test.
func Object(data []byte) Input {
	return Input{Name: "obj.o", Data: data}
}

// AsmSource is assembly for the harness to assemble and link.
func AsmSource(text string) Input {
	return Input{Name: "src.s", Data: []byte(text)}
}

// CSource is C for the harness to compile and link, such as a driver whose main
// calls the generated code and reports what it found.
func CSource(text string) Input {
	return Input{Name: "src.c", Data: []byte(text)}
}

// Result is what running an image produced.
type Result struct {
	// Exit is the process's exit status.
	Exit int

	// Stdout and Stderr are everything the process wrote. Tests that check
	// printed output read these; the rest get them quoted in failure messages.
	Stdout string
	Stderr string

	// Bin is the image's path. It lives in the test's temporary directory, so it
	// survives until the test finishes and can be inspected on failure.
	Bin string

	// Runner records how the image was executed, so a test can report whether an
	// emulator was involved.
	Runner Runner
}

// WithLinkArgs returns a copy of h with extra arguments appended to the link
// command. Use it for a per-test need -- a driver that wants -lpthread -- rather
// than declaring a second near-identical harness.
func (h Harness) WithLinkArgs(extra ...string) Harness {
	combined := make([]string, 0, len(h.LinkArgs)+len(extra))
	combined = append(combined, h.LinkArgs...)
	combined = append(combined, extra...)
	h.LinkArgs = combined
	return h
}

// Run builds an image from inputs and executes it, returning what it produced.
//
// Tools are resolved before any work happens -- compiler, linker, then the runner
// -- so a host that cannot complete the whole round trip skips instead of failing
// partway through something it was never able to finish.
func (h Harness) Run(t testing.TB, inputs ...Input) Result {
	t.Helper()
	compiler, linker := h.tools(t)
	runner := h.Machine.Runner(t)
	bin := h.build(t, compiler, linker, inputs)

	cmd := runner.Command(bin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	result := Result{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
		Bin:    bin,
		Runner: runner,
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		// A non-zero status is the normal way these images report a result, not a
		// failure of the harness.
		result.Exit = exitErr.ExitCode()
		return result
	}
	if err != nil {
		// Anything else -- the image did not start, a signal killed it, the
		// emulator refused it -- is not a result the test can interpret.
		t.Fatalf("run %s %s: %v\nstdout: %q\nstderr: %q",
			bin, runner.Describe(), err, result.Stdout, result.Stderr)
	}
	return result
}

// Exit is [Harness.Run] reduced to the exit status, for the many tests that
// encode their whole verdict in it.
func (h Harness) Exit(t testing.TB, inputs ...Input) int {
	t.Helper()
	return h.Run(t, inputs...).Exit
}

// Require resolves everything getting an image to run takes -- the compiler, the
// linker, and the runner -- and returns the runner, without building anything.
//
// [Harness.Run] does this for itself, so most tests never call it. Call it first
// in a test that does substantial work before it has an object to hand over,
// notably one that runs a compiler frontend: the frontend has its own opinions
// about the host toolchain, and without this gate a machine missing a tool fails
// somewhere inside that earlier work instead of skipping, which reads as a broken
// test rather than an incomplete toolchain.
func (h Harness) Require(t testing.TB) Runner {
	t.Helper()
	h.tools(t)
	return h.Machine.Runner(t)
}

// Build compiles and links inputs into an image and returns its path, without
// running it. It is [Harness.Run] without the run, for a test that inspects the
// linked image rather than its behaviour.
func (h Harness) Build(t testing.TB, inputs ...Input) string {
	t.Helper()
	compiler, linker := h.tools(t)
	return h.build(t, compiler, linker, inputs)
}

// tools resolves the compiler and the linker, skipping (or, under
// CG12_REQUIRE_TOOLS, failing) when neither of the named alternatives is present.
func (h Harness) tools(t testing.TB) (compiler, linker string) {
	t.Helper()
	if len(h.Compiler) > 0 {
		compiler = testenv.Tool(t, h.Compiler...)
	}
	if len(h.Linker) > 0 {
		linker = testenv.Tool(t, h.Linker...)
	}
	return compiler, linker
}

// build writes every input to a fresh temporary directory, compiles the sources
// among them, links the lot, and returns the image's path.
func (h Harness) build(t testing.TB, compiler, linker string, inputs []Input) string {
	t.Helper()
	dir := t.TempDir()

	// The stub defines the entry point, so it leads the link line: a linker
	// picking an entry by position finds it, and one picking _start by name is
	// unaffected either way.
	all := inputs
	if h.StartStub != "" {
		stub := AsmSource(h.StartStub)
		stub.Name = "start.s"
		all = append([]Input{stub}, inputs...)
	}

	var objects []string
	for i, in := range all {
		// Inputs are named by the caller and several may share a name (two
		// objects, say), so the index keeps the files apart.
		path := filepath.Join(dir, fmt.Sprintf("%d-%s", i, in.Name))
		err := os.WriteFile(path, in.Data, 0o644)
		if err != nil {
			t.Fatalf("write %s: %v", in.Name, err)
		}
		if strings.HasSuffix(in.Name, ".o") {
			objects = append(objects, path)
			continue
		}
		objects = append(objects, h.compile(t, compiler, dir, path, in))
	}

	bin := filepath.Join(dir, "image")
	args := append([]string{}, h.LinkArgs...)
	args = append(args, "-o", bin)
	args = append(args, objects...)
	out, err := exec.Command(linker, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("link %s: %v\n%s%s", strings.Join(args, " "), err, out, diagnose(all))
	}
	return bin
}

// compile turns one source input into an object and returns its path.
func (h Harness) compile(t testing.TB, compiler, dir, path string, in Input) string {
	t.Helper()
	object := path + ".o"
	args := append([]string{}, h.CompileArgs...)
	args = append(args, "-c", path, "-o", object)
	out, err := exec.Command(compiler, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("compile %s: %v\n%s%s", in.Name, err, out, diagnose([]Input{in}))
	}
	return object
}

// diagnose collects whatever the inputs can say about themselves, for a failure
// message. It returns the empty string when none of them can say anything, so the
// common case adds no noise.
func diagnose(inputs []Input) string {
	var b strings.Builder
	for _, in := range inputs {
		if in.Diagnose == nil {
			continue
		}
		fmt.Fprintf(&b, "\n--- %s ---\n%s", in.Name, in.Diagnose())
	}
	return b.String()
}
