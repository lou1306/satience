package fuzzer

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"satience/internal/cnf"
	"satience/internal/parser"
	"satience/internal/solver"
)

type FuzzResult struct {
	InstanceName string
	Variables    int
	Clauses      int
	Density      float64
	Seed         int64
	Result       solver.SolveResult
	Iterations   int
	Conflicts    int
	Decisions    int
	ModelValid   bool
	Error        error
}

type Fuzzer struct {
	rng       *rand.Rand
	tempDir   string
	seed      int64
	verbose   bool
}

func New(seed int64, verbose bool) *Fuzzer {
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	return &Fuzzer{
		rng:     rand.New(rand.NewSource(seed)),
		tempDir: filepath.Join(os.TempDir(), "satience-fuzz"),
		seed:    seed,
		verbose: verbose,
	}
}

func (f *Fuzzer) log(format string, args ...interface{}) {
	if f.verbose {
		fmt.Printf(format, args...)
	}
}

func (f *Fuzzer) GenerateRandomCNF(numVars, numClauses int, seed int64) *cnf.CNF {
	rng := rand.New(rand.NewSource(seed))
	formula := &cnf.CNF{
		NumVars:    uint32(numVars),
		NumClauses: numClauses,
		Clauses:    make([]cnf.Clause, 0, numClauses),
	}

	for i := 0; i < numClauses; i++ {
		clauseSize := 3
		if numVars < 3 {
			clauseSize = numVars
		}

		literals := make(map[uint32]bool)
		clauseLits := make([]cnf.Literal, 0, clauseSize)

		for len(literals) < clauseSize {
			varIdx := rng.Intn(numVars)
			isNegated := rng.Intn(2) == 1

			var lit cnf.Literal
			if isNegated {
				lit = cnf.NewLiteral(uint32(varIdx), true)
			} else {
				lit = cnf.NewLiteral(uint32(varIdx), false)
			}

			if !literals[uint32(varIdx)] {
				literals[uint32(varIdx)] = true
				clauseLits = append(clauseLits, lit)
			}
		}

		formula.Clauses = append(formula.Clauses, cnf.Clause{Literals: clauseLits})
	}

	return formula
}

func (f *Fuzzer) GenerateStructuredInstance(instanceType string, size int, seed int64) (*cnf.CNF, error) {
	switch instanceType {
	case "chain":
		return f.generateChain(size)
	case "xor":
		return f.generateXOR(size)
	case "atmostone":
		return f.generateAtMostOne(size)
	case "pigeonhole":
		return f.generatePigeonhole(size, size-1)
	default:
		return nil, fmt.Errorf("unknown instance type: %s", instanceType)
	}
}

func (f *Fuzzer) generateChain(size int) (*cnf.CNF, error) {
	clauses := make([]cnf.Clause, 0, size*2)

	for i := 0; i < size; i++ {
		xi := cnf.NewLiteral(uint32(i), false)
		xiNeg := cnf.NewLiteral(uint32(i), true)

		if i < size-1 {
			xi1 := cnf.NewLiteral(uint32(i+1), false)
			clauses = append(clauses, cnf.Clause{Literals: []cnf.Literal{xiNeg, xi1}})
		}

		if i == 0 {
			clauses = append(clauses, cnf.Clause{Literals: []cnf.Literal{xi}})
		}
	}

	return &cnf.CNF{
		NumVars:    uint32(size),
		NumClauses: len(clauses),
		Clauses:    clauses,
	}, nil
}

func (f *Fuzzer) generateXOR(size int) (*cnf.CNF, error) {
	clauses := make([]cnf.Clause, 0)

	for i := 0; i < size-1; i++ {
		xi := cnf.NewLiteral(uint32(i), false)
		xiNeg := cnf.NewLiteral(uint32(i), true)
		xi1 := cnf.NewLiteral(uint32(i+1), false)
		xi1Neg := cnf.NewLiteral(uint32(i+1), true)

		clauses = append(clauses, cnf.Clause{Literals: []cnf.Literal{xi, xi1}})
		clauses = append(clauses, cnf.Clause{Literals: []cnf.Literal{xiNeg, xi1Neg}})
	}

	return &cnf.CNF{
		NumVars:    uint32(size),
		NumClauses: len(clauses),
		Clauses:    clauses,
	}, nil
}

func (f *Fuzzer) generateAtMostOne(size int) (*cnf.CNF, error) {
	clauses := make([]cnf.Clause, 0)

	for i := 0; i < size; i++ {
		for j := i + 1; j < size; j++ {
			xiNeg := cnf.NewLiteral(uint32(i), true)
			xjNeg := cnf.NewLiteral(uint32(j), true)
			clauses = append(clauses, cnf.Clause{Literals: []cnf.Literal{xiNeg, xjNeg}})
		}
	}

	return &cnf.CNF{
		NumVars:    uint32(size),
		NumClauses: len(clauses),
		Clauses:    clauses,
	}, nil
}

func (f *Fuzzer) generatePigeonhole(pigeons, holes int) (*cnf.CNF, error) {
	if pigeons <= holes {
		return nil, fmt.Errorf("pigeons must be greater than holes for UNSAT instance")
	}

	numVars := pigeons * holes
	clauses := make([]cnf.Clause, 0)

	getVar := func(p, h int) int {
		return p*holes + h
	}

	for p := 0; p < pigeons; p++ {
		clauseLits := make([]cnf.Literal, holes)
		for h := 0; h < holes; h++ {
			clauseLits[h] = cnf.NewLiteral(uint32(getVar(p, h)), false)
		}
		clauses = append(clauses, cnf.Clause{Literals: clauseLits})
	}

	for h := 0; h < holes; h++ {
		for p1 := 0; p1 < pigeons; p1++ {
			for p2 := p1 + 1; p2 < pigeons; p2++ {
				xp1hNeg := cnf.NewLiteral(uint32(getVar(p1, h)), true)
				xp2hNeg := cnf.NewLiteral(uint32(getVar(p2, h)), true)
				clauses = append(clauses, cnf.Clause{Literals: []cnf.Literal{xp1hNeg, xp2hNeg}})
			}
		}
	}

	return &cnf.CNF{
		NumVars:    uint32(numVars),
		NumClauses: len(clauses),
		Clauses:    clauses,
	}, nil
}

func (f *Fuzzer) WriteCNF(formula *cnf.CNF, filename string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	fmt.Fprintf(file, "p cnf %d %d\n", formula.NumVars, formula.NumClauses)
	for _, clause := range formula.Clauses {
		for _, lit := range clause.Literals {
			varVal := lit.Var() + 1
			if lit.IsNegated() {
				fmt.Fprintf(file, "-%d ", varVal)
			} else {
				fmt.Fprintf(file, "%d ", varVal)
			}
		}
		fmt.Fprintln(file, "0")
	}

	return nil
}

func (f *Fuzzer) assignmentsToModel(formula *cnf.CNF, assignments []solver.Assignment) []cnf.Literal {
	model := make([]cnf.Literal, 0, formula.NumVars)
	for i := range assignments {
		if assignments[i].Level > 0 {
			varIdx := uint32(i)
			if assignments[i].Value {
				model = append(model, cnf.NewLiteral(varIdx, false))
			} else {
				model = append(model, cnf.NewLiteral(varIdx, true))
			}
		}
	}
	return model
}

func (f *Fuzzer) VerifyModel(formula *cnf.CNF, model []cnf.Literal) bool {
	assignment := make([]int, formula.NumVars+1)
	for _, lit := range model {
		varIdx := int(lit.Var()) + 1
		if lit.IsNegated() {
			assignment[varIdx] = -1
		} else {
			assignment[varIdx] = 1
		}
	}

	for _, clause := range formula.Clauses {
		satisfied := false
		for _, lit := range clause.Literals {
			varIdx := int(lit.Var()) + 1
			if varIdx > len(assignment) {
				return false
			}
			litIsNeg := lit.IsNegated()
			val := assignment[varIdx]

			if (litIsNeg && val == -1) || (!litIsNeg && val == 1) {
				satisfied = true
				break
			}
		}
		if !satisfied {
			return false
		}
	}

	return true
}

func (f *Fuzzer) RunFuzzTest(formula *cnf.CNF, name string, timeoutSec int) FuzzResult {
	filename := filepath.Join(f.tempDir, name+".cnf")
	if err := f.WriteCNF(formula, filename); err != nil {
		return FuzzResult{
			InstanceName: name,
			Error:        fmt.Errorf("failed to write CNF: %w", err),
		}
	}

	file, err := os.Open(filename)
	if err != nil {
		return FuzzResult{
			InstanceName: name,
			Error:        fmt.Errorf("failed to open CNF: %w", err),
		}
	}
	defer file.Close()

	parsed, err := parser.Parse(file)
	if err != nil {
		return FuzzResult{
			InstanceName: name,
			Error:        fmt.Errorf("failed to parse CNF: %w", err),
		}
	}

	s := solver.NewCDCLSolver(parsed)
	s.SetVerbose(false)

	startTime := time.Now()
	result := s.Solve()
	elapsed := time.Since(startTime)

	if timeoutSec > 0 && elapsed.Seconds() > float64(timeoutSec) {
		return FuzzResult{
			InstanceName: name,
			Variables:    int(parsed.NumVars),
			Clauses:      parsed.NumClauses,
			Result:       solver.UNKNOWN,
			Error:        fmt.Errorf("timeout after %.2fs", elapsed.Seconds()),
		}
	}

	modelValid := true
	resultType := solver.UNSAT
	if result {
		resultType = solver.SAT
		assignments := s.GetAssignments()
		model := f.assignmentsToModel(parsed, assignments)
		modelValid = f.VerifyModel(parsed, model)
		if !modelValid {
			return FuzzResult{
				InstanceName: name,
				Variables:    int(parsed.NumVars),
				Clauses:      parsed.NumClauses,
				Result:       resultType,
				ModelValid:   false,
				Error:        fmt.Errorf("model does not satisfy all clauses"),
			}
		}
	}

	stats := s.GetStats()
	density := float64(parsed.NumClauses) / float64(parsed.NumVars)

	return FuzzResult{
		InstanceName: name,
		Variables:    int(parsed.NumVars),
		Clauses:      parsed.NumClauses,
		Density:      density,
		Result:       resultType,
		Iterations:   stats["iterations"],
		Conflicts:    stats["conflicts"],
		Decisions:    stats["decisions"],
		ModelValid:   modelValid,
		Error:        nil,
	}
}

func (f *Fuzzer) RunRandomFuzz(numTests int, maxVars, maxClauses int, timeoutSec int) []FuzzResult {
	results := make([]FuzzResult, 0, numTests)

	if err := os.MkdirAll(f.tempDir, 0755); err != nil {
		return append(results, FuzzResult{Error: fmt.Errorf("failed to create temp dir: %w", err)})
	}

	f.log("Starting random fuzzing: %d tests, max %d vars, max %d clauses\n", numTests, maxVars, maxClauses)

	for i := 0; i < numTests; i++ {
		numVars := f.rng.Intn(maxVars-10) + 10
		numClauses := f.rng.Intn(maxClauses-numVars) + numVars
		seed := f.rng.Int63()

		formula := f.GenerateRandomCNF(numVars, numClauses, seed)
		name := fmt.Sprintf("fuzz_random_v%d_c%d_s%d", numVars, numClauses, seed)

		result := f.RunFuzzTest(formula, name, timeoutSec)
		result.Seed = seed
		results = append(results, result)

		if result.Error != nil {
			f.log("  [%d] %s: ERROR - %v\n", i+1, name, result.Error)
		} else {
			resultStr := "SAT"
			if result.Result == solver.UNSAT {
				resultStr = "UNSAT"
			}
			f.log("  [%d] %s: %s (%d conflicts, %d decisions)\n", i+1, name, resultStr, result.Conflicts, result.Decisions)
		}

		os.Remove(filepath.Join(f.tempDir, name+".cnf"))
	}

	return results
}

func (f *Fuzzer) RunStructuredFuzz(numTests int, timeoutSec int) []FuzzResult {
	results := make([]FuzzResult, 0, numTests)

	if err := os.MkdirAll(f.tempDir, 0755); err != nil {
		return append(results, FuzzResult{Error: fmt.Errorf("failed to create temp dir: %w", err)})
	}

	instanceTypes := []string{"chain", "xor", "atmostone"}

	f.log("Starting structured fuzzing: %d tests\n", numTests)

	for i := 0; i < numTests; i++ {
		instanceType := instanceTypes[f.rng.Intn(len(instanceTypes))]
		size := f.rng.Intn(90) + 10
		seed := f.rng.Int63()

		formula, err := f.GenerateStructuredInstance(instanceType, size, seed)
		if err != nil {
			results = append(results, FuzzResult{
				InstanceName: fmt.Sprintf("fuzz_%s_%d", instanceType, size),
				Error:        err,
			})
			continue
		}

		name := fmt.Sprintf("fuzz_%s_v%d_s%d", instanceType, size, seed)
		result := f.RunFuzzTest(formula, name, timeoutSec)
		result.Seed = seed
		results = append(results, result)

		if result.Error != nil {
			f.log("  [%d] %s: ERROR - %v\n", i+1, name, result.Error)
		} else {
			resultStr := "SAT"
			if result.Result == solver.UNSAT {
				resultStr = "UNSAT"
			}
			f.log("  [%d] %s: %s (%d conflicts, %d decisions)\n", i+1, name, resultStr, result.Conflicts, result.Decisions)
		}

		os.Remove(filepath.Join(f.tempDir, name+".cnf"))
	}

	return results
}

func (f *Fuzzer) RunPigeonholeFuzz(maxPigeons int, timeoutSec int) []FuzzResult {
	results := make([]FuzzResult, 0)

	if err := os.MkdirAll(f.tempDir, 0755); err != nil {
		return append(results, FuzzResult{Error: fmt.Errorf("failed to create temp dir: %w", err)})
	}

	f.log("Starting pigeonhole fuzzing: up to %d pigeons\n", maxPigeons)

	for pigeons := 3; pigeons <= maxPigeons; pigeons++ {
		holes := pigeons - 1
		formula, err := f.generatePigeonhole(pigeons, holes)
		if err != nil {
			f.log("  Skipping %dp%dh: %v\n", pigeons, holes, err)
			continue
		}

		name := fmt.Sprintf("fuzz_php_%dp%dh", pigeons, holes)
		result := f.RunFuzzTest(formula, name, timeoutSec)

		if result.Error != nil {
			f.log("  %s: ERROR - %v\n", name, result.Error)
		} else {
			resultStr := "UNSAT"
			if result.Result == solver.SAT {
				resultStr = "SAT (unexpected)"
			}
			f.log("  %s: %s (%d conflicts, %d decisions)\n", name, resultStr, result.Conflicts, result.Decisions)
		}

		results = append(results, result)
		os.Remove(filepath.Join(f.tempDir, name+".cnf"))
	}

	return results
}

func PrintSummary(results []FuzzResult) {
	total := len(results)
	sat := 0
	unsat := 0
	errors := 0
	modelInvalid := 0

	for _, r := range results {
		if r.Error != nil {
			errors++
		} else if r.Result == solver.SAT {
			sat++
			if !r.ModelValid {
				modelInvalid++
			}
		} else if r.Result == solver.UNSAT {
			unsat++
		}
	}

	fmt.Println("\n=== Fuzzing Summary ===")
	fmt.Printf("Total tests:     %d\n", total)
	fmt.Printf("SAT:             %d\n", sat)
	fmt.Printf("UNSAT:           %d\n", unsat)
	fmt.Printf("Errors:          %d\n", errors)
	fmt.Printf("Invalid models:  %d\n", modelInvalid)
	fmt.Printf("Success rate:    %.1f%%\n", float64(total-errors)/float64(total)*100)
	if sat > 0 {
		fmt.Printf("Soundness:       %.1f%% (models verified)\n", float64(sat-modelInvalid)/float64(sat)*100)
	}

	if modelInvalid > 0 {
		fmt.Println("\n⚠️  CRITICAL: Some models do not satisfy all clauses!")
	}
	if errors > 0 {
		fmt.Println("\n⚠️  Some tests encountered errors:")
		for _, r := range results {
			if r.Error != nil {
				fmt.Printf("   - %s: %v\n", r.InstanceName, r.Error)
			}
		}
	}
}

func HasCriticalErrors(results []FuzzResult) bool {
	for _, r := range results {
		if r.Error != nil || !r.ModelValid {
			return true
		}
	}
	return false
}
