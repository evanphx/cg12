package main

type deferMethodRecorder struct {
	values []int
}

func (recorder *deferMethodRecorder) Add(value int) {
	recorder.values = append(recorder.values, value)
}

func main() {
	recorder := &deferMethodRecorder{}
	record := recorder.Add

	func() {
		defer record(1)
		defer recorder.Add(2)
		defer record(3)
	}()

	if len(recorder.values) != 3 {
		panic("wrong number of deferred method calls")
	}
	if recorder.values[0] != 3 || recorder.values[1] != 2 || recorder.values[2] != 1 {
		panic("deferred method values ran in wrong order")
	}
}
