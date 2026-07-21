package main

import "runtime/metrics"

func main() {
	descriptions := metrics.All()
	if len(descriptions) == 0 {
		panic("metrics All returned no descriptions")
	}

	samples := []metrics.Sample{
		{Name: "/gc/cycles/total:gc-cycles"},
		{Name: "/memory/classes/heap/free:bytes"},
	}
	metrics.Read(samples)
	if samples[0].Value.Kind() == metrics.KindBad {
		panic("metrics gc cycles sample bad")
	}
	if samples[1].Value.Kind() == metrics.KindBad {
		panic("metrics heap free sample bad")
	}
}
