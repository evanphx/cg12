package main

import "flag"

func main() {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	name := flags.String("name", "default", "name value")
	count := flags.Int("count", 0, "count value")
	enabled := flags.Bool("enabled", false, "enabled value")

	err := flags.Parse([]string{"-name=cg12", "-count", "17", "-enabled", "extra"})
	if err != nil {
		panic("flag Parse failed")
	}

	if *name != "cg12" {
		panic("flag string mismatch")
	}
	if *count != 17 {
		panic("flag int mismatch")
	}
	if !*enabled {
		panic("flag bool mismatch")
	}
	if flags.NArg() != 1 || flags.Arg(0) != "extra" {
		panic("flag args mismatch")
	}
}
