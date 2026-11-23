// mutant-schemata: margin(scripts/mutate-par.py)用。stdin = 变异的清单(JSON),stdout = 合编的结果(JSON)。
//
//	usage: mutant-schemata ROOT OUT < mutants.json
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"apifactory/factory/mutant"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: mutant-schemata ROOT OUT < mutants.json")
		os.Exit(2)
	}
	var muts []mutant.Mutant
	if err := json.NewDecoder(os.Stdin).Decode(&muts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	res, err := mutant.Build(os.Args[1], os.Args[2], muts)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(res)
}
