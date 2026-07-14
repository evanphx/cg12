//go:build linux

package bpf

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// This file loads a compiled Object into the kernel: it creates the maps,
// patches their file descriptors into the program's relocated map-fd loads, and
// asks the verifier to accept the program. It can then run the program with a
// synthetic context (BPF_PROG_TEST_RUN) and read the maps back — enough to
// generate and exercise a real eBPF program without external tooling. Loading
// requires CAP_BPF (or root).

// bpf command numbers.
const (
	cmdMapCreate   = 0
	cmdMapLookup   = 1
	cmdMapUpdate   = 2
	cmdProgLoad    = 5
	cmdProgTestRun = 10
	cmdMapFreeze   = 22
)

// sysBPFNum is the bpf(2) syscall number per architecture.
var sysBPFNum = map[string]uintptr{"amd64": 321, "arm64": 280, "386": 357, "arm": 386}

func bpf(cmd int, attr []byte) (uintptr, syscall.Errno) {
	nr, ok := sysBPFNum[runtime.GOARCH]
	if !ok {
		return 0, syscall.ENOSYS
	}
	r, _, e := syscall.Syscall(nr, uintptr(cmd), uintptr(unsafe.Pointer(&attr[0])), uintptr(len(attr)))
	runtime.KeepAlive(attr)
	return r, e
}

func ptr(b []byte) uint64 {
	if len(b) == 0 {
		return 0
	}
	return uint64(uintptr(unsafe.Pointer(&b[0])))
}

// LoadedObject holds the kernel file descriptors of a loaded program and its
// maps. Close it to release them.
type LoadedObject struct {
	Prog  int            // the first program's fd (the entry, for single-program objects)
	Maps  map[string]int // map name -> fd
	Progs map[string]int // program name -> fd (every entry program in the object)
}

// Close releases the program and map file descriptors.
func (l *LoadedObject) Close() {
	for _, fd := range l.Progs {
		syscall.Close(fd)
	}
	for _, fd := range l.Maps {
		syscall.Close(fd)
	}
}

// CreateMap creates a map in the kernel and returns its file descriptor.
func CreateMap(m MapDef) (int, error) {
	attr := make([]byte, 64)
	binary.LittleEndian.PutUint32(attr[0:], m.Type)
	binary.LittleEndian.PutUint32(attr[4:], m.KeySize)
	binary.LittleEndian.PutUint32(attr[8:], m.ValueSize)
	binary.LittleEndian.PutUint32(attr[12:], m.MaxEntries)
	binary.LittleEndian.PutUint32(attr[16:], m.Flags)
	name := m.Name
	if len(name) > 15 {
		name = name[:15]
	}
	copy(attr[28:44], name) // map_name[16]
	fd, errno := bpf(cmdMapCreate, attr)
	if errno != 0 {
		return -1, fmt.Errorf("bpf map create %q: %w", m.Name, errno)
	}
	return int(fd), nil
}

// MapLookup reads the value at key into value; both slices must match the map's
// key and value sizes.
func MapLookup(fd int, key, value []byte) error {
	attr := make([]byte, 32)
	binary.LittleEndian.PutUint32(attr[0:], uint32(fd))
	binary.LittleEndian.PutUint64(attr[8:], ptr(key))
	binary.LittleEndian.PutUint64(attr[16:], ptr(value))
	if _, errno := bpf(cmdMapLookup, attr); errno != 0 {
		return fmt.Errorf("bpf map lookup: %w", errno)
	}
	return nil
}

// MapUpdate writes value at key.
func MapUpdate(fd int, key, value []byte, flags uint64) error {
	attr := make([]byte, 32)
	binary.LittleEndian.PutUint32(attr[0:], uint32(fd))
	binary.LittleEndian.PutUint64(attr[8:], ptr(key))
	binary.LittleEndian.PutUint64(attr[16:], ptr(value))
	binary.LittleEndian.PutUint64(attr[24:], flags)
	if _, errno := bpf(cmdMapUpdate, attr); errno != 0 {
		return fmt.Errorf("bpf map update: %w", errno)
	}
	return nil
}

// Freeze marks a map read-only to eBPF programs (they may then load values from
// it as known constants). Used for .rodata.
func Freeze(fd int) error {
	attr := make([]byte, 16)
	binary.LittleEndian.PutUint32(attr[0:], uint32(fd))
	if _, errno := bpf(cmdMapFreeze, attr); errno != 0 {
		return fmt.Errorf("bpf map freeze: %w", errno)
	}
	return nil
}

// LoadProgram asks the verifier to load raw bytecode of the given program type
// under the given license (empty defaults to GPL), returning its fd or the
// verifier log on rejection.
func LoadProgram(progType uint32, code []byte, license string) (int, string, error) {
	if license == "" {
		license = "GPL"
	}
	lic := append([]byte(license), 0)
	log := make([]byte, 1<<16)
	attr := make([]byte, 120)
	binary.LittleEndian.PutUint32(attr[0:], progType)
	binary.LittleEndian.PutUint32(attr[4:], uint32(len(code)/8))
	binary.LittleEndian.PutUint64(attr[8:], ptr(code))
	binary.LittleEndian.PutUint64(attr[16:], ptr(lic))
	binary.LittleEndian.PutUint32(attr[24:], 1) // log_level
	binary.LittleEndian.PutUint32(attr[28:], uint32(len(log)))
	binary.LittleEndian.PutUint64(attr[32:], ptr(log))
	fd, errno := bpf(cmdProgLoad, attr)
	logStr := nulTrim(log)
	if errno != 0 {
		return -1, logStr, fmt.Errorf("bpf prog load: %w", errno)
	}
	return int(fd), logStr, nil
}

// Load creates the object's maps, patches their descriptors into the first
// program's map-fd loads, and loads that program. On a verifier rejection the
// error includes the verifier log.
func Load(obj *Object, progType uint32) (*LoadedObject, error) {
	if len(obj.Programs) == 0 {
		return nil, fmt.Errorf("bpf: object has no program")
	}
	maps := map[string]int{}
	for _, m := range obj.Maps {
		fd, err := CreateMap(m)
		if err != nil {
			closeAll(maps)
			return nil, err
		}
		if m.Initial != nil { // .rodata/.data: seed the single entry
			key := make([]byte, 4)
			if err := MapUpdate(fd, key, m.Initial, 0); err != nil {
				closeAll(maps)
				return nil, err
			}
			if m.Frozen { // .rodata is frozen read-only for the verifier
				if err := Freeze(fd); err != nil {
					closeAll(maps)
					return nil, err
				}
			}
		}
		maps[m.Name] = fd
	}

	progs := map[string]int{}
	var first int
	for i, prog := range obj.Programs {
		insns := append([]Insn(nil), prog.Insns...)
		for _, r := range prog.Relocs {
			fd, ok := maps[r.Map]
			if !ok {
				closeProgs(progs)
				closeAll(maps)
				return nil, fmt.Errorf("bpf: program %q references unknown map %q", prog.Name, r.Map)
			}
			insns[r.Insn].Imm = int32(fd) // the ld_imm64's first slot carries the fd
		}
		fd, log, err := LoadProgram(progType, (&Prog{Insns: insns}).Bytes(), obj.License)
		if err != nil {
			closeProgs(progs)
			closeAll(maps)
			return nil, fmt.Errorf("loading program %q: %w\n%s", prog.Name, err, log)
		}
		progs[prog.Name] = fd
		if i == 0 {
			first = fd
		}
	}
	return &LoadedObject{Prog: first, Maps: maps, Progs: progs}, nil
}

// TestRun executes the first loaded program repeat times with dataIn as its
// context input, returning the program's return value.
func (l *LoadedObject) TestRun(dataIn []byte, repeat uint32) (retval uint32, err error) {
	return l.testRunFD(l.Prog, dataIn, repeat)
}

// TestRunProg runs a specific named program from the object.
func (l *LoadedObject) TestRunProg(name string, dataIn []byte, repeat uint32) (uint32, error) {
	fd, ok := l.Progs[name]
	if !ok {
		return 0, fmt.Errorf("bpf: no loaded program named %q", name)
	}
	return l.testRunFD(fd, dataIn, repeat)
}

func (l *LoadedObject) testRunFD(progFD int, dataIn []byte, repeat uint32) (retval uint32, err error) {
	dataOut := make([]byte, len(dataIn)+256)
	attr := make([]byte, 64)
	binary.LittleEndian.PutUint32(attr[0:], uint32(progFD))
	binary.LittleEndian.PutUint32(attr[8:], uint32(len(dataIn)))   // data_size_in
	binary.LittleEndian.PutUint32(attr[12:], uint32(len(dataOut))) // data_size_out
	binary.LittleEndian.PutUint64(attr[16:], ptr(dataIn))
	binary.LittleEndian.PutUint64(attr[24:], ptr(dataOut))
	binary.LittleEndian.PutUint32(attr[32:], repeat)
	if _, errno := bpf(cmdProgTestRun, attr); errno != 0 {
		return 0, fmt.Errorf("bpf prog test run: %w", errno)
	}
	return binary.LittleEndian.Uint32(attr[4:]), nil // retval
}

func closeAll(fds map[string]int) {
	for _, fd := range fds {
		syscall.Close(fd)
	}
}

func closeProgs(fds map[string]int) {
	for _, fd := range fds {
		syscall.Close(fd)
	}
}

func nulTrim(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
