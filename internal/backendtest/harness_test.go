package backendtest

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The harness is tested with stand-in tools rather than a real toolchain, and no
// backend at all: what it has to get right is which files it compiles, what it
// puts on the link line and in which order, and that it reports back the exit
// status and the output the image produced. None of that needs a compiler that
// truly compiles, and pretending otherwise would make these tests skip on the
// machines most likely to break them.

// fakeToolchain installs a stand-in compiler and linker on PATH and returns a
// harness using them, plus the path of the file the linker logs its arguments to.
//
// The stand-ins treat shell script as their object format: the compiler copies its
// input, the linker concatenates its inputs and marks the result executable. So an
// "image" here is the sources pasted together, and running it runs them.
func fakeToolchain(t *testing.T) (Harness, string) {
	t.Helper()
	dir := t.TempDir()

	compiler := filepath.Join(dir, "cg12-fake-cc")
	writeScript(t, compiler, `#!/bin/sh
# Accepts the harness's compile line -- [flags] -c SRC -o OBJ -- and treats
# compiling as copying.
while [ $# -gt 0 ]; do
	case "$1" in
		-c) src="$2"; shift 2 ;;
		-o) obj="$2"; shift 2 ;;
		*) shift ;;
	esac
done
cp "$src" "$obj"
`)

	logPath := filepath.Join(dir, "link.log")
	linker := filepath.Join(dir, "cg12-fake-ld")
	writeScript(t, linker, `#!/bin/sh
# Accepts [flags] -o IMAGE OBJ... , records the whole line for the test to
# check, and links by concatenation.
echo "$@" >> "$CG12_FAKE_LD_LOG"
objs=""
while [ $# -gt 0 ]; do
	case "$1" in
		-o) out="$2"; shift 2 ;;
		-*) shift ;;
		*) objs="$objs $1"; shift ;;
	esac
done
cat $objs > "$out"
chmod +x "$out"
`)

	t.Setenv("CG12_FAKE_LD_LOG", logPath)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	harness := Harness{
		// The host's own architecture, so the runner is native and these tests
		// need no emulator.
		Machine:     Machine{Name: "host", GOARCH: runtime.GOARCH},
		Compiler:    []string{"cg12-fake-cc"},
		CompileArgs: []string{"--target=host"},
		Linker:      []string{"cg12-fake-ld"},
		LinkArgs:    []string{"-static", "-nostdlib"},
		StartStub:   "#!/bin/sh\n",
	}
	return harness, logPath
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	err := os.WriteFile(path, []byte(body), 0o755)
	if err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// The round trip: sources compiled, everything linked, the image run, and its
// exit status and output handed back.
func TestRunReportsExitAndOutput(t *testing.T) {
	harness, _ := fakeToolchain(t)
	result := harness.Run(t, CSource("echo to-stdout\necho to-stderr 1>&2\nexit 7\n"))

	if result.Exit != 7 {
		t.Errorf("Exit = %d, want 7", result.Exit)
	}
	if result.Stdout != "to-stdout\n" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "to-stdout\n")
	}
	if result.Stderr != "to-stderr\n" {
		t.Errorf("Stderr = %q, want %q", result.Stderr, "to-stderr\n")
	}
	if !result.Runner.Native() {
		t.Errorf("image for the host's own architecture ran under %q", result.Runner.Emulator)
	}
	if _, err := os.Stat(result.Bin); err != nil {
		t.Errorf("the image is not on disk for the test to inspect: %v", err)
	}
}

// The same round trip for a machine this host is not: the image goes to the
// emulator, and the emulator's exit status is the result. Checked end to end
// rather than only on the Runner, because "resolved an emulator" and "actually ran
// the image through it" are different claims and only the second one is the point.
func TestRunUsesTheEmulatorForAForeignMachine(t *testing.T) {
	harness, _ := fakeToolchain(t)
	// A stand-in emulator: it records that it ran and then runs the image, the
	// way a user-mode emulator does.
	dir := t.TempDir()
	marker := filepath.Join(dir, "emulated")
	emulator := filepath.Join(dir, "cg12-fake-emulator")
	writeScript(t, emulator, `#!/bin/sh
echo "$1" > "$CG12_FAKE_EMULATOR_MARKER"
exec "$@"
`)
	t.Setenv("CG12_FAKE_EMULATOR_MARKER", marker)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	harness.Machine = Machine{
		Name:      "foreign",
		GOARCH:    "absentarch",
		Emulators: []string{"cg12-fake-emulator"},
	}

	result := harness.Run(t, CSource("echo emulated-output\nexit 4\n"))
	if result.Exit != 4 {
		t.Errorf("Exit = %d, want 4", result.Exit)
	}
	if result.Stdout != "emulated-output\n" {
		t.Errorf("Stdout = %q, want the image's output", result.Stdout)
	}
	if result.Runner.Native() {
		t.Error("a foreign machine's image reported a native run")
	}
	ranWith, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the emulator never ran: %v", err)
	}
	if strings.TrimSpace(string(ranWith)) != result.Bin {
		t.Errorf("the emulator was handed %q, want the image %q", strings.TrimSpace(string(ranWith)), result.Bin)
	}
}

// Exit is the same thing without the ceremony, which is how nearly every backend
// test uses the harness.
func TestExitIsRunsStatus(t *testing.T) {
	harness, _ := fakeToolchain(t)
	if code := harness.Exit(t, CSource("exit 0\n")); code != 0 {
		t.Errorf("Exit = %d, want 0", code)
	}
	if code := harness.Exit(t, CSource("exit 3\n")); code != 3 {
		t.Errorf("Exit = %d, want 3", code)
	}
}

// The link line: the configured LinkArgs are passed, the output comes after them,
// and the entry stub leads the objects. The order matters to a real linker
// choosing an entry point, and nothing else in the suite would notice if it
// changed.
func TestLinkLineShape(t *testing.T) {
	harness, logPath := fakeToolchain(t)
	harness.Run(t, CSource("exit 0\n"))

	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read link log: %v", err)
	}
	line := strings.TrimSpace(string(logged))
	fields := strings.Fields(line)
	if len(fields) < 6 {
		t.Fatalf("link line is too short to be right: %q", line)
	}
	if fields[0] != "-static" || fields[1] != "-nostdlib" {
		t.Errorf("link line does not start with the harness's LinkArgs: %q", line)
	}
	if fields[2] != "-o" {
		t.Errorf("link line does not name the output after the flags: %q", line)
	}
	stub, source := fields[4], fields[5]
	if !strings.HasSuffix(stub, "start.s.o") {
		t.Errorf("first object is %q, want the compiled entry stub", stub)
	}
	if !strings.HasSuffix(source, "src.c.o") {
		t.Errorf("second object is %q, want the compiled source", source)
	}
}

// An object input is linked as it stands. A backend's output arrives this way, so
// a harness that ran it through the compiler first would be testing the compiler.
func TestObjectInputsAreNotCompiled(t *testing.T) {
	harness, logPath := fakeToolchain(t)
	// The "object" is the body of the script the fake linker will assemble.
	harness.Run(t, Object([]byte("exit 5\n")))

	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read link log: %v", err)
	}
	if strings.Contains(string(logged), "obj.o.o") {
		t.Errorf("the object was compiled instead of linked directly: %q", logged)
	}
	if !strings.Contains(string(logged), "obj.o") {
		t.Errorf("the object never reached the link line: %q", logged)
	}
	if code := harness.Exit(t, Object([]byte("exit 5\n"))); code != 5 {
		t.Errorf("Exit = %d, want 5", code)
	}
}

// Several inputs sharing a name -- two objects, the ordinary case for a backend
// test that links a helper alongside its own output -- must not overwrite each
// other.
func TestInputsWithTheSameNameCoexist(t *testing.T) {
	harness, _ := fakeToolchain(t)
	first := Object([]byte("first=1\n"))
	second := Object([]byte("exit $((first + 1))\n"))
	if code := harness.Exit(t, first, second); code != 2 {
		t.Errorf("Exit = %d, want 2: one input overwrote the other", code)
	}
}

// A harness without a stub links only what it was given, which is how a suite
// that gets its entry point from libc's crt0 uses it.
func TestNoStartStubLinksOnlyTheInputs(t *testing.T) {
	harness, logPath := fakeToolchain(t)
	harness.StartStub = ""
	harness.Run(t, CSource("#!/bin/sh\nexit 0\n"))

	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read link log: %v", err)
	}
	if strings.Contains(string(logged), "start.s") {
		t.Errorf("a stub was linked into a harness that has none: %q", logged)
	}
}

// WithLinkArgs appends without disturbing the harness it was called on, so a test
// borrowing the package's shared harness for one extra flag cannot change it for
// every other test in the file.
func TestWithLinkArgsDoesNotMutateTheOriginal(t *testing.T) {
	harness, _ := fakeToolchain(t)
	extended := harness.WithLinkArgs("-lpthread")

	want := []string{"-static", "-nostdlib", "-lpthread"}
	if strings.Join(extended.LinkArgs, " ") != strings.Join(want, " ") {
		t.Errorf("extended LinkArgs = %v, want %v", extended.LinkArgs, want)
	}
	if strings.Join(harness.LinkArgs, " ") != "-static -nostdlib" {
		t.Errorf("the original harness was modified: %v", harness.LinkArgs)
	}
	// Appending twice from the same base must not have the two results share
	// backing array capacity.
	other := harness.WithLinkArgs("-lm")
	if strings.Join(extended.LinkArgs, " ") != strings.Join(want, " ") {
		t.Errorf("a second WithLinkArgs overwrote the first: %v", extended.LinkArgs)
	}
	if strings.Join(other.LinkArgs, " ") != "-static -nostdlib -lm" {
		t.Errorf("second extended LinkArgs = %v", other.LinkArgs)
	}
}

// The input constructors say what they are by their file extension, which is what
// the harness dispatches on.
func TestInputConstructors(t *testing.T) {
	if got := Object([]byte("x")).Name; !strings.HasSuffix(got, ".o") {
		t.Errorf("Object name = %q, want an object extension", got)
	}
	if got := AsmSource("x").Name; !strings.HasSuffix(got, ".s") {
		t.Errorf("AsmSource name = %q, want an assembly extension", got)
	}
	if got := CSource("x").Name; !strings.HasSuffix(got, ".c") {
		t.Errorf("CSource name = %q, want a C extension", got)
	}
	if got := string(CSource("int main(void){}").Data); got != "int main(void){}" {
		t.Errorf("CSource data = %q", got)
	}
}

// Diagnose is only for failure messages, so it must be quiet when nothing has one
// to give and must name the input when it does.
func TestDiagnose(t *testing.T) {
	plain := []Input{Object([]byte("x")), CSource("y")}
	if got := diagnose(plain); got != "" {
		t.Errorf("diagnose of plain inputs = %q, want nothing", got)
	}

	described := Object([]byte("x"))
	described.Diagnose = func() string { return "movq %rax, %rbx" }
	got := diagnose([]Input{described})
	if !strings.Contains(got, "obj.o") || !strings.Contains(got, "movq %rax, %rbx") {
		t.Errorf("diagnose = %q, want it to name the input and its description", got)
	}
}

// The freestanding x86-64 configuration is what every amd64 execution test uses,
// and getting its machine wrong would send them all through an emulator (or all
// to a skip). Checked here so the mistake surfaces even on a host that cannot
// build an x86-64 image at all.
func TestX8664FreestandingConfiguration(t *testing.T) {
	harness := X8664Freestanding()
	if harness.Machine.GOARCH != "amd64" {
		t.Errorf("machine GOARCH = %q, want amd64", harness.Machine.GOARCH)
	}
	if strings.Join(harness.Machine.Emulators, " ") != "qemu-x86_64" {
		t.Errorf("emulators = %v, want qemu-x86_64", harness.Machine.Emulators)
	}
	if strings.Join(harness.LinkArgs, " ") != "-static -nostdlib" {
		t.Errorf("link args = %v, want a freestanding static link", harness.LinkArgs)
	}
	if !strings.Contains(strings.Join(harness.CompileArgs, " "), "x86_64") {
		t.Errorf("compile args = %v, want an x86-64 target flag", harness.CompileArgs)
	}
	if !strings.Contains(harness.StartStub, "call runtest") {
		t.Errorf("the entry stub does not call runtest:\n%s", harness.StartStub)
	}
	// Each call must hand back a harness the caller can adjust without affecting
	// anyone else's, which is why this is a function and not a package variable.
	first := X8664Freestanding().WithLinkArgs("-z", "noexecstack")
	if len(X8664Freestanding().LinkArgs) != len(first.LinkArgs)-2 {
		t.Error("adjusting one harness changed the shared configuration")
	}
}
