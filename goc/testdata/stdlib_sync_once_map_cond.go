package main

import "sync"

func main() {
	var once sync.Once
	count := 0
	for i := 0; i < 5; i++ {
		once.Do(func() {
			count++
		})
	}
	if count != 1 {
		panic("sync.Once mismatch")
	}

	var values sync.Map
	values.Store("alpha", 17)
	values.Store("beta", 25)
	if value, ok := values.Load("alpha"); !ok || value.(int) != 17 {
		panic("sync.Map Load mismatch")
	}
	values.Delete("alpha")
	if _, ok := values.Load("alpha"); ok {
		panic("sync.Map Delete mismatch")
	}

	mutex := new(sync.Mutex)
	condition := sync.NewCond(mutex)
	ready := false
	done := make(chan bool, 1)

	go func() {
		condition.L.Lock()
		for !ready {
			condition.Wait()
		}
		condition.L.Unlock()
		done <- true
	}()

	condition.L.Lock()
	ready = true
	condition.Signal()
	condition.L.Unlock()

	if !<-done {
		panic("sync.Cond mismatch")
	}
}
