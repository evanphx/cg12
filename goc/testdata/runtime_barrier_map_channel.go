// Map and channel pointer write barriers.
//
// RUNTIME_PLAN.md section 6 lists both shapes. Neither one is an ordinary
// pointer store in the generated code: the compiler hands the value to a
// runtime helper, and the barrier lives inside that helper, so these are tests
// that the helper is being reached with the right type rather than that a store
// was barriered.
//
//   - a map assignment goes through the map runtime, which copies the key and
//     the element into a slot that the map may later move during growth. Both
//     keys and elements are pointerful here, and the map is grown well past its
//     initial size so evacuation runs while the collector is marking.
//   - a channel send copies the element into the buffer, into a receiver's
//     stack slot for an unbuffered send, or into a sudog. Buffered, unbuffered
//     and select shapes are all covered.
//
// The capability runs under GODEBUG=cg12checkwb=2.
package main

import (
	"runtime"
	"runtime/debug"
)

type key struct {
	tag  int64
	note *note
}

type note struct {
	tag int64
}

type value struct {
	tag   int64
	inner *note
}

const rounds = 64
const entries = 128
const messages = 256

var maps []map[key]*value
var mapKeys [][]key
var deletedWitnesses []*value
var received []*value

func collectInBackground(done chan struct{}) {
	for {
		select {
		case <-done:
			close(done)
			return
		default:
			runtime.GC()
		}
	}
}

// buildMap fills a map whose keys and elements both contain pointers. The keys
// are retained because a key's note field is compared by pointer identity, so
// looking an entry up again means presenting the same key value.
func buildMap(base int64) (map[key]*value, []key) {
	built := make(map[key]*value)
	keys := make([]key, entries)
	for index := 0; index < entries; index++ {
		tag := base + int64(index)
		keys[index] = key{tag: tag, note: &note{tag: tag}}
		built[keys[index]] = &value{tag: tag, inner: &note{tag: tag + 1}}
	}
	// Overwrite half the entries so an existing slot's previous element is
	// replaced, which is the deletion half of the barrier.
	for index := 0; index < entries/2; index++ {
		tag := base + int64(index)
		replaced := built[keys[index]]
		if replaced == nil {
			panic("a map entry could not be found by an equal key")
		}
		deletedWitnesses = append(deletedWitnesses, replaced)
		built[keys[index]] = &value{tag: tag + 10000, inner: &note{tag: tag + 10001}}
	}
	// Delete a quarter of them, which clears their slots.
	for index := entries / 2; index < entries*3/4; index++ {
		delete(built, keys[index])
	}
	return built, keys
}

func checkMap(built map[key]*value, keys []key, base int64) {
	if len(built) != entries-entries/4 {
		panic("a map has the wrong size")
	}
	for index := 0; index < entries; index++ {
		tag := base + int64(index)
		found, present := built[keys[index]]
		if index >= entries/2 && index < entries*3/4 {
			if present {
				panic("a deleted map entry is still present")
			}
			continue
		}
		if !present || found == nil {
			panic("a map entry was lost")
		}
		expected := tag
		if index < entries/2 {
			expected = tag + 10000
		}
		if found.tag != expected {
			panic("a map element lost its value")
		}
		if found.inner == nil || found.inner.tag != expected+1 {
			panic("a map element lost a referent")
		}
	}
}

func sendAndReceive(base int64) {
	buffered := make(chan *value, 16)
	unbuffered := make(chan *value)
	done := make(chan struct{})

	go func() {
		for message := range buffered {
			received = append(received, message)
		}
		for message := range unbuffered {
			received = append(received, message)
		}
		close(done)
	}()

	for index := 0; index < messages; index++ {
		tag := base + int64(index)
		buffered <- &value{tag: tag, inner: &note{tag: tag + 1}}
	}
	close(buffered)
	for index := 0; index < messages; index++ {
		tag := base + int64(index) + 500000
		message := &value{tag: tag, inner: &note{tag: tag + 1}}
		select {
		case unbuffered <- message:
		}
	}
	close(unbuffered)
	<-done
}

func main() {
	debug.SetGCPercent(1)
	stop := make(chan struct{})
	go collectInBackground(stop)

	for round := 0; round < rounds; round++ {
		base := int64(round) * 100000
		built, keys := buildMap(base)
		checkMap(built, keys, base)
		maps = append(maps, built)
		mapKeys = append(mapKeys, keys)
	}
	sendAndReceive(1)

	stop <- struct{}{}
	<-stop

	runtime.GC()
	runtime.GC()

	if len(maps) != rounds {
		panic("the map list has the wrong length")
	}
	for round, built := range maps {
		checkMap(built, mapKeys[round], int64(round)*100000)
	}
	for index, witness := range deletedWitnesses {
		if witness == nil {
			panic("a replaced map element became nil")
		}
		round := int64(index / (entries / 2))
		offset := int64(index % (entries / 2))
		if witness.tag != round*100000+offset {
			panic("a replaced map element was freed and reused")
		}
	}
	if len(received) != 2*messages {
		panic("the receiver did not see every message")
	}
	for index := 0; index < messages; index++ {
		message := received[index]
		if message == nil || message.tag != 1+int64(index) {
			panic("a buffered channel message lost its value")
		}
		if message.inner == nil || message.inner.tag != message.tag+1 {
			panic("a buffered channel message lost a referent")
		}
	}
	for index := 0; index < messages; index++ {
		message := received[messages+index]
		if message == nil || message.tag != 1+int64(index)+500000 {
			panic("an unbuffered channel message lost its value")
		}
		if message.inner == nil || message.inner.tag != message.tag+1 {
			panic("an unbuffered channel message lost a referent")
		}
	}
}
