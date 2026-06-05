package solver

import (
	"fmt"
	"satience/internal/cnf"
)

// SolveResult represents the result of solving
type SolveResult int

const (
	SAT SolveResult = iota
	UNSAT
	UNKNOWN
)

// CDCLSolver implements a CDCL solver (DPLL with VSIDS + clause learning)
type CDCLSolver struct {
	cnf          *cnf.CNF
	assignments  []Assignment
	trail        []int
	trailHead    []int
	level        int
	vsids        *VSIDS
	conflicts    int
	implication  []int
	iterations   int
	maxIter      int
	learnedClauses []cnf.Clause      // Original clauses (keep for compatibility)
	learnedArena *cnf.ClauseArena    // Arena-based learned clause storage
	clauseActivity []float64
	clauseAge    []int
	clauseSize   []int // Track clause size for deletion
	currentAge   int
	verbose      bool
	decisions    int
	backjumpLevel int
	maxLearned   int
	minLearned   int // Minimum clauses to keep (aggressive deletion target)
	savedPhase   []bool
	restartBase  int
	restartCount int
	lubyIndex    int
	lbdSum       int
	lbdCount     int
	lastConflictLBD int
	conflictsAtLevel []int  // Track conflicts per decision level
	lastRandomDecision int  // Last conflict where we made random decision
}

// NewCDCLSolver creates a new CDCL solver (DPLL with VSIDS)
func NewCDCLSolver(formula *cnf.CNF) *CDCLSolver {
	maxLearned := 500   // Allow more accumulation between restarts
	minLearned := 200   // Target after deletion (60% reduction)
	restartBase := 100  // Base for Luby restart sequence
	return &CDCLSolver{
		cnf:         formula,
		assignments: make([]Assignment, formula.NumVars),
		trail:       make([]int, 0),
		trailHead:   make([]int, 1),
		level:       0,
		vsids:       NewVSIDS(formula.NumVars),
		conflicts:   0,
		implication: make([]int, formula.NumVars),
		iterations:  0,
		maxIter:     0,
		learnedClauses: make([]cnf.Clause, 0),
		learnedArena: cnf.NewClauseArena(1000), // Reduced arena size
		clauseActivity: make([]float64, 0),
		clauseAge:    make([]int, 0),
		currentAge:   0,
		verbose:     false,
		decisions:   0,
		backjumpLevel: 0,
		maxLearned:   maxLearned,
		minLearned:   minLearned,
		savedPhase:  make([]bool, formula.NumVars),
		restartBase:  restartBase,
		restartCount: 0,
		lubyIndex:    0,
		lbdSum:       0,
		lbdCount:     0,
		lastConflictLBD: 0,
		conflictsAtLevel: make([]int, formula.NumVars+1),
		lastRandomDecision: -1000,
	}
}

// SetMaxIter sets the maximum iteration limit (0 = unlimited)
func (s *CDCLSolver) SetMaxIter(limit int) {
	s.maxIter = limit
}

// SetVerbose enables/disables verbose output
func (s *CDCLSolver) SetVerbose(v bool) {
	s.verbose = v
}

// EnableLRB enables LRB (Learning Rate Based) heuristic
func (s *CDCLSolver) EnableLRB() {
	s.vsids.EnableLRB()
}

// GetStats returns solving statistics
func (s *CDCLSolver) GetStats() map[string]int {
	return map[string]int{
		"conflicts":     s.conflicts,
		"decisions":     s.decisions,
		"iterations":    s.iterations,
		"learned":       len(s.learnedClauses),
		"level":         s.level,
	}
}

func (s *CDCLSolver) printStats() {
	fmt.Printf("c \n")
	fmt.Printf("c === Solving Statistics ===\n")
	fmt.Printf("c Variables:     %d\n", s.cnf.NumVars)
	fmt.Printf("c Clauses:       %d\n", s.cnf.NumClauses)
	fmt.Printf("c Conflicts:     %d\n", s.conflicts)
	fmt.Printf("c Decisions:     %d\n", s.decisions)
	fmt.Printf("c Iterations:    %d\n", s.iterations)
	fmt.Printf("c Learned:       %d\n", len(s.learnedClauses))
	fmt.Printf("c Max Level:     %d\n", s.level)
	fmt.Printf("c \n")
}

func (s *CDCLSolver) preprocessAggressive() SolveResult {
	if s.verbose {
		fmt.Printf("c [verbose] Aggressive preprocessing: %d variables, %d clauses\n", s.cnf.NumVars, s.cnf.NumClauses)
	}

	initialClauses := s.cnf.NumClauses
	
	for pass := 0; pass < 5; pass++ {
		if s.verbose {
			fmt.Printf("c [verbose] Preprocessing pass %d: %d clauses\n", pass+1, s.cnf.NumClauses)
		}
		
		unitResult := s.unitPropagationPreprocess()
		if unitResult != UNKNOWN {
			return unitResult
		}
		
		pureResult := s.pureLiteralElimination()
		if pureResult != UNKNOWN {
			return pureResult
		}
		
		s.selfSubsumption()
		
		s.hyperBinaryResolution()
		
		equivResult := s.equivalenceDetection()
		if equivResult != UNKNOWN {
			return equivResult
		}
		
		if s.conflicts < 1000 {
			failedResult := s.failedLiteralElimination()
			if failedResult != UNKNOWN {
				return failedResult
			}
		}
		
		veResult := s.variableElimination()
		if veResult != UNKNOWN {
			return veResult
		}
		
		if s.cnf.NumClauses == initialClauses && pass >= 2 {
			break
		}
		initialClauses = s.cnf.NumClauses
	}
	
	bceResult := s.blockedClauseElimination()
	if bceResult != UNKNOWN {
		return bceResult
	}
	
	if s.verbose {
		fmt.Printf("c [verbose] After preprocessing: %d variables, %d clauses\n", s.cnf.NumVars, s.cnf.NumClauses)
	}
	
	return UNKNOWN
}

func (s *CDCLSolver) selfSubsumption() {
	changed := true
	for changed {
		changed = false
		for i := 0; i < len(s.cnf.Clauses); i++ {
			for j := 0; j < len(s.cnf.Clauses); j++ {
				if i == j {
					continue
				}
				
				clauseA := s.cnf.Clauses[i]
				clauseB := s.cnf.Clauses[j]
				
				if len(clauseA.Literals) != 2 || len(clauseB.Literals) < 2 {
					continue
				}
				
				for _, litA := range clauseA.Literals {
					for _, litB := range clauseB.Literals {
						if litA.Var() == litB.Var() && litA.IsNegated() != litB.IsNegated() {
						resolvent := s.resolveOnVar(clauseA, clauseB, litA.Var())
						if resolvent != nil && s.subsumes(resolvent, &s.cnf.Clauses[j]) {
							s.cnf.Clauses[j] = *resolvent
							changed = true
							if s.verbose {
								fmt.Printf("c [verbose] Self-subsumption: strengthened clause\n")
							}
						}
							goto nextPair
						}
					}
				}
				nextPair:
			}
		}
	}
}

func (s *CDCLSolver) hyperBinaryResolution() {
	binaryUnits := make(map[uint32]bool)
	
	for _, clause := range s.cnf.Clauses {
		if len(clause.Literals) == 2 {
			lit1, lit2 := clause.Literals[0], clause.Literals[1]
			if s.isUnitLiteral(lit1) {
				binaryUnits[lit1.Var()] = !lit1.IsNegated()
			}
			if s.isUnitLiteral(lit2) {
				binaryUnits[lit2.Var()] = !lit2.IsNegated()
			}
		}
	}
	
	if len(binaryUnits) == 0 {
		return
	}
	
	for i := 0; i < len(s.cnf.Clauses); i++ {
		clause := s.cnf.Clauses[i]
		if len(clause.Literals) < 3 {
			continue
		}
		
		newLiterals := make([]cnf.Literal, 0)
		for _, lit := range clause.Literals {
			if assigned, exists := binaryUnits[lit.Var()]; exists {
				litTrue := !lit.IsNegated()
				if litTrue == assigned {
					goto satisfied
				}
			} else {
				newLiterals = append(newLiterals, lit)
			}
		}
		
		if len(newLiterals) == 0 {
			if s.verbose {
				fmt.Printf("c [verbose] Hyper-binary: empty clause\n")
			}
			return
		}
		
		s.cnf.Clauses[i] = cnf.Clause{Literals: newLiterals, Learned: false}
	satisfied:
	}
	
	newClauses := make([]cnf.Clause, 0)
	for _, clause := range s.cnf.Clauses {
		satisfied := false
		for _, lit := range clause.Literals {
			if assigned, exists := binaryUnits[lit.Var()]; exists {
				litTrue := !lit.IsNegated()
				if litTrue == assigned {
					satisfied = true
					break
				}
			}
		}
		if !satisfied {
			newClauses = append(newClauses, clause)
		}
	}
	s.cnf.Clauses = newClauses
	s.cnf.NumClauses = len(newClauses)
}

func (s *CDCLSolver) subsumptionElimination() {
	removed := 0
	
	for i := 0; i < len(s.cnf.Clauses); i++ {
		for j := 0; j < len(s.cnf.Clauses); j++ {
			if i == j {
				continue
			}
			
			if s.subsumes(&s.cnf.Clauses[i], &s.cnf.Clauses[j]) {
				s.cnf.Clauses[j] = s.cnf.Clauses[len(s.cnf.Clauses)-1]
				s.cnf.Clauses = s.cnf.Clauses[:len(s.cnf.Clauses)-1]
				removed++
				if j < len(s.cnf.Clauses) {
					j--
				}
			}
		}
	}
	
	if s.verbose && removed > 0 {
		fmt.Printf("c [verbose] Subsumption elimination: removed %d clauses\n", removed)
	}
}

func (s *CDCLSolver) subsumeLearnedClauses(newClause *cnf.Clause) {
	// Remove learned clauses that are subsumed by the new clause
	remaining := make([]cnf.Clause, 0, len(s.learnedClauses))
	removed := 0
	
	for i := range s.learnedClauses {
		if !s.subsumes(newClause, &s.learnedClauses[i]) {
			remaining = append(remaining, s.learnedClauses[i])
		} else {
			removed++
		}
	}
	
	if removed > 0 {
		s.learnedClauses = remaining
		s.clauseActivity = make([]float64, len(s.learnedClauses))
		s.clauseAge = make([]int, len(s.learnedClauses))
		if s.verbose {
			fmt.Printf("c [verbose] Learned clause subsumption: removed %d clauses\n", removed)
		}
	}
}

func (s *CDCLSolver) isSubsumedByAny(clause cnf.Clause, clauses []cnf.Clause) bool {
	for _, other := range clauses {
		if s.subsumes(&other, &clause) {
			return true
		}
	}
	return false
}

func (s *CDCLSolver) failedLiteralElimination() SolveResult {
	if s.verbose {
		fmt.Printf("c [verbose] Failed literal elimination: checking %d variables\n", s.cnf.NumVars)
	}
	
	changed := true
	for changed {
		changed = false
		
		for varIdx := uint32(0); varIdx < s.cnf.NumVars; varIdx++ {
			if s.assignments[varIdx].Level != 0 {
				continue
			}
			
			for polarity := 0; polarity < 2; polarity++ {
				value := polarity == 0
				lit := cnf.NewLiteral(varIdx, !value)
				
				savedTrail := len(s.trail)
				savedImplication := make([]int, len(s.implication))
				copy(savedImplication, s.implication)
				
				s.assignLiteral(lit, 1, -1)
				conflict, _ := s.propagate()
				
				if conflict {
					oppositeValue := !value
					s.assignments[varIdx] = Assignment{
						Value: oppositeValue,
						Level: 1,
					}
					s.trail = s.trail[:savedTrail]
					s.trailHead = s.trailHead[:1]
					s.level = 0
					copy(s.implication, savedImplication)
					
					conflict = s.simplifyAfterAssignment(varIdx, oppositeValue)
					if conflict {
						return UNSAT
					}
					
					changed = true
					if s.verbose {
						fmt.Printf("c [verbose] Failed literal: var %d = %v\n", varIdx, oppositeValue)
					}
					break
				} else {
					for i := savedTrail; i < len(s.trail); i++ {
						v := uint32(s.trail[i])
						s.assignments[v] = Assignment{}
						s.implication[v] = savedImplication[v]
					}
					s.trail = s.trail[:savedTrail]
					s.trailHead = s.trailHead[:1]
					s.level = 0
				}
			}
		}
	}
	
	return UNKNOWN
}

func (s *CDCLSolver) isUnitLiteral(lit cnf.Literal) bool {
	varIdx := lit.Var()
	for _, clause := range s.cnf.Clauses {
		if len(clause.Literals) == 1 && clause.Literals[0].Var() == varIdx {
			return true
		}
	}
	return false
}

func (s *CDCLSolver) resolveOnVar(c1, c2 cnf.Clause, varIdx uint32) *cnf.Clause {
	literals := make([]cnf.Literal, 0)
	foundNeg := false
	foundPos := false
	
	for _, lit := range c1.Literals {
		if lit.Var() == varIdx {
			if lit.IsNegated() {
				foundNeg = true
			} else {
				foundPos = true
			}
		} else {
			literals = append(literals, lit)
		}
	}
	
	for _, lit := range c2.Literals {
		if lit.Var() == varIdx {
			if lit.IsNegated() {
				foundNeg = true
			} else {
				foundPos = true
			}
		} else {
			literals = append(literals, lit)
		}
	}
	
	if !(foundNeg && foundPos) {
		return nil
	}
	
	return &cnf.Clause{Literals: literals, Learned: false}
}

func (s *CDCLSolver) subsumes(c1, c2 *cnf.Clause) bool {
	if len(c1.Literals) >= len(c2.Literals) {
		return false
	}
	
	set := make(map[uint32]bool)
	for _, lit := range c1.Literals {
		key := uint32(lit)<<1 | boolToUint(lit.IsNegated())
		set[key] = true
	}
	
	for _, lit := range c2.Literals {
		key := uint32(lit)<<1 | boolToUint(lit.IsNegated())
		if !set[key] {
			return false
		}
	}
	return true
}

func boolToUint(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

func luby(i int) int {
	k := 1
	for {
		ki := 1 << uint(k)
		if i == ki-1 {
			return 1 << uint(k-1)
		}
		if ki-1 > i {
			prevKi := 1 << uint(k-1)
			return luby(i - (prevKi - 1))
		}
		k++
	}
}

func (s *CDCLSolver) shouldRestart() bool {
	// Very aggressive restarts for structured instances
	// PHP and similar instances benefit from restarts every 100 conflicts
	aggressiveThreshold := 100
	
	conflictsSinceRestart := s.conflicts - s.restartCount
	
	// First, check if we should use aggressive restarts
	// Detect structured instances by high conflict rate at low decision levels
	if s.conflicts > 500 && s.level <= 8 {
		// Structured instance: use very frequent restarts
		if conflictsSinceRestart >= aggressiveThreshold {
			return true
		}
	}
	
	// Hybrid restart strategy: Luby + adaptive (Glucose-style)
	// For the first 100 conflicts, use Luby to collect statistics
	if s.lbdCount < 100 {
		threshold := s.restartBase * luby(s.lubyIndex + 1)
		return conflictsSinceRestart >= threshold
	}
	
	// After 100 conflicts, use adaptive restarts based on LBD
	// Restart if current LBD is much worse than average
	avgLBD := float64(s.lbdSum) / float64(s.lbdCount)
	if s.lastConflictLBD > int(2.0*avgLBD) {
		return true
	}
	
	// Fallback to Luby for periodic restarts
	threshold := s.restartBase * luby(s.lubyIndex + 1)
	return conflictsSinceRestart >= threshold
}

func (s *CDCLSolver) restart() {
	if s.verbose {
		fmt.Printf("c [verbose] Restart #%d at conflict %d\n", s.lubyIndex+1, s.conflicts)
	}
	
	// IMPORTANT: Calculate LBD and identify glue clauses BEFORE clearing assignments!
	// Glue clauses (LBD <= 3) are preserved across restarts
	glueCount := 0
	isGlue := make([]bool, len(s.learnedClauses))
	
	for i, clause := range s.learnedClauses {
		// Calculate LBD while assignments are still valid
		levelSet := make(map[int]bool)
		for _, lit := range clause.Literals {
			lvl := s.assignments[lit.Var()].Level
			if lvl > 0 {
				levelSet[lvl] = true
			}
		}
		lbd := len(levelSet)
		
		// Keep glue clauses (LBD <= 5) - relaxed threshold
		// LBD <= 2: core glue (most valuable)
		// LBD 3-5: useful clauses (keep across restarts)
		// LBD > 5: trash (delete)
		if lbd <= 5 {
			glueCount++
			isGlue[i] = true
		}
	}
	
	if s.verbose {
		fmt.Printf("c [verbose] Restart: keeping %d glue clauses, deleting %d non-glue\n", glueCount, len(s.learnedClauses)-glueCount)
	}
	
	// Clear trail and assignments
	s.trail = s.trail[:0]
	s.trailHead = s.trailHead[:1]
	s.level = 0
	for i := range s.implication {
		s.implication[i] = -1
	}
	for i := range s.assignments {
		s.assignments[i] = Assignment{}
	}
	// Reset conflicts at all levels
	for i := range s.conflictsAtLevel {
		s.conflictsAtLevel[i] = 0
	}
	
	// Inprocessing: apply subsumption elimination periodically
	if s.conflicts > 0 && s.conflicts % 500 == 0 {
		s.inprocessing()
	}
	
	// Compact to keep only glue clauses
	newClauses := make([]cnf.Clause, 0, glueCount)
	newActivity := make([]float64, 0, glueCount)
	newAge := make([]int, 0, glueCount)
	newSize := make([]int, 0, glueCount)
	
	for i := range s.learnedClauses {
		if isGlue[i] {
			newClauses = append(newClauses, s.learnedClauses[i])
			newActivity = append(newActivity, s.clauseActivity[i])
			newAge = append(newAge, s.clauseAge[i])
			newSize = append(newSize, s.clauseSize[i])
		}
	}
	
	s.learnedClauses = newClauses
	s.clauseActivity = newActivity
	s.clauseAge = newAge
	s.clauseSize = newSize
	
	// Rebuild arena with only glue clauses
	s.learnedArena.Reset()
	for _, clause := range s.learnedClauses {
		s.learnedArena.AllocateClause(clause.Literals, true)
	}
	
	s.lubyIndex++
	s.restartCount = s.conflicts
	s.lbdSum = 0
	s.lbdCount = 0
	s.lastConflictLBD = 0
	s.backjumpLevel = 0
}

func (s *CDCLSolver) variableElimination() SolveResult {
	if s.verbose {
		fmt.Printf("c [verbose] Variable elimination: checking %d variables\n", s.cnf.NumVars)
	}
	
	eliminatedCount := 0
	resolventCount := 0
	
	changed := true
	for changed {
		changed = false
		eliminated := make([]bool, s.cnf.NumVars)
		
		for varIdx := uint32(0); varIdx < s.cnf.NumVars; varIdx++ {
			if eliminated[varIdx] {
				continue
			}
			
			posClauses := make([]int, 0)
			negClauses := make([]int, 0)
			
			for i, clause := range s.cnf.Clauses {
				hasPos := false
				hasNeg := false
				for _, lit := range clause.Literals {
					if lit.Var() == varIdx {
						if !lit.IsNegated() {
							hasPos = true
						} else {
							hasNeg = true
						}
					}
				}
				
				if hasPos {
					posClauses = append(posClauses, i)
				}
				if hasNeg {
					negClauses = append(negClauses, i)
				}
			}
			
			if len(posClauses) == 0 || len(negClauses) == 0 {
				continue
			}
			
			resolvents := make([]cnf.Clause, 0)
			resolventSet := make(map[string]bool)
			
			for _, posIdx := range posClauses {
				posClause := s.cnf.Clauses[posIdx]
				
				for _, negIdx := range negClauses {
					negClause := s.cnf.Clauses[negIdx]
					
					resolvent := s.resolveForElimination(posClause, negClause, varIdx)
					if resolvent != nil {
						if !s.isTautology(resolvent) {
							key := s.clauseKey(resolvent)
							if !resolventSet[key] {
								resolventSet[key] = true
								resolvents = append(resolvents, *resolvent)
							}
						}
					}
				}
			}
			
			originalCount := len(posClauses) + len(negClauses)
			// More aggressive elimination: allow up to 50% blowup
			// This enables elimination on PHP and other structured instances
			// where resolvents can be reused for multiple eliminations
			maxResolvents := originalCount + (originalCount / 2)
			if len(resolvents) <= maxResolvents {
				if s.verbose {
					fmt.Printf("c [verbose] Eliminating var %d: %d clauses -> %d resolvents\n", 
						varIdx, originalCount, len(resolvents))
				}
				
				for _, resolvent := range resolvents {
					if len(resolvent.Literals) == 0 {
						if s.verbose {
							fmt.Printf("c [verbose] Variable elimination: empty clause created (UNSAT)\n")
						}
						return UNSAT
					}
				}
				
				keepClauses := make([]cnf.Clause, 0)
				for _, clause := range s.cnf.Clauses {
					keep := true
					for _, lit := range clause.Literals {
						if lit.Var() == varIdx {
							keep = false
							break
						}
					}
					if keep {
						keepClauses = append(keepClauses, clause)
					}
				}
				
				for _, resolvent := range resolvents {
					if !s.isSubsumedByAny(resolvent, keepClauses) {
						keepClauses = append(keepClauses, resolvent)
					}
				}
				
				s.cnf.Clauses = keepClauses
				s.cnf.NumClauses = len(keepClauses)
				
				eliminated[varIdx] = true
				eliminatedCount++
				resolventCount += len(resolvents)
				changed = true
			}
		}
	}
	
	if s.verbose {
		fmt.Printf("c [verbose] Variable elimination: eliminated %d variables, added %d resolvents\n", 
			eliminatedCount, resolventCount)
	}
	
	if len(s.cnf.Clauses) == 0 {
		if s.verbose {
			fmt.Printf("c [verbose] Variable elimination: all clauses satisfied\n")
		}
		return SAT
	}
	
	return UNKNOWN
}

func (s *CDCLSolver) resolveForElimination(clause1, clause2 cnf.Clause, varIdx uint32) *cnf.Clause {
	hasPosX := false
	hasNegX := false
	
	for _, lit := range clause1.Literals {
		if lit.Var() == varIdx && !lit.IsNegated() {
			hasPosX = true
			break
		}
	}
	
	for _, lit := range clause2.Literals {
		if lit.Var() == varIdx && lit.IsNegated() {
			hasNegX = true
			break
		}
	}
	
	if !hasPosX || !hasNegX {
		return nil
	}
	
	resolventLits := make([]cnf.Literal, 0, len(clause1.Literals)+len(clause2.Literals)-2)
	
	for _, lit := range clause1.Literals {
		if lit.Var() != varIdx {
			resolventLits = append(resolventLits, lit)
		}
	}
	
	for _, lit := range clause2.Literals {
		if lit.Var() != varIdx {
			resolventLits = append(resolventLits, lit)
		}
	}
	
	if len(resolventLits) == 0 {
		return &cnf.Clause{Literals: make([]cnf.Literal, 0), Learned: false}
	}
	
	return &cnf.Clause{Literals: resolventLits, Learned: false}
}

func (s *CDCLSolver) isTautology(clause *cnf.Clause) bool {
	seen := make(map[uint32]bool)
	
	for _, lit := range clause.Literals {
		varIdx := lit.Var()
		isNeg := lit.IsNegated()
		
		if prevNeg, exists := seen[varIdx]; exists {
			if prevNeg != isNeg {
				return true
			}
		} else {
			seen[varIdx] = isNeg
		}
	}
	
	return false
}

func (s *CDCLSolver) clauseKey(clause *cnf.Clause) string {
	lits := make([]uint64, len(clause.Literals))
	for i, lit := range clause.Literals {
		lits[i] = uint64(lit)
	}
	
	for i := 0; i < len(lits)-1; i++ {
		for j := i + 1; j < len(lits); j++ {
			if lits[i] > lits[j] {
				lits[i], lits[j] = lits[j], lits[i]
			}
		}
	}
	
	return fmt.Sprintf("%v", lits)
}

func (s *CDCLSolver) blockedClauseElimination() SolveResult {
	if s.cnf.NumClauses > 5000 {
		if s.verbose {
			fmt.Printf("c [verbose] Blocked clause elimination: skipped (%d clauses, limit 5000)\n", s.cnf.NumClauses)
		}
		return UNKNOWN
	}

	if s.verbose {
		fmt.Printf("c [verbose] Blocked clause elimination: checking %d clauses\n", s.cnf.NumClauses)
	}

	removedCount := 0
	changed := true

	for changed {
		changed = false
		blocked := make([]bool, len(s.cnf.Clauses))

		for clauseIdx, clause := range s.cnf.Clauses {
			if len(clause.Literals) == 0 {
				return UNSAT
			}

			for _, blockingLit := range clause.Literals {
				if s.isClauseBlockedBy(clause, blockingLit) {
					blocked[clauseIdx] = true
					removedCount++
					changed = true
					break
				}
			}
		}

		if changed {
			remaining := make([]cnf.Clause, 0)
			for i, clause := range s.cnf.Clauses {
				if !blocked[i] {
					remaining = append(remaining, clause)
				}
			}
			s.cnf.Clauses = remaining
			s.cnf.NumClauses = len(remaining)
		}
	}

	if s.verbose {
		fmt.Printf("c [verbose] Blocked clause elimination: removed %d clauses\n", removedCount)
	}

	return UNKNOWN
}

func (s *CDCLSolver) isClauseBlockedBy(clause cnf.Clause, blockingLit cnf.Literal) bool {
	opposite := blockingLit.Negate()

	for _, other := range s.cnf.Clauses {
		containsOpposite := false
		for _, lit := range other.Literals {
			if lit == opposite {
				containsOpposite = true
				break
			}
		}

		if !containsOpposite {
			continue
		}

		resolvent := s.resolveOnVar(clause, other, blockingLit.Var())

		if resolvent != nil && !s.isTautology(resolvent) {
			return false
		}
	}

	return true
}

func (s *CDCLSolver) inprocessing() {
	if s.verbose {
		fmt.Printf("c [verbose] Inprocessing: %d conflicts, %d clauses\n", s.conflicts, s.cnf.NumClauses)
	}
	
	initialClauses := s.cnf.NumClauses
	
	s.subsumptionElimination()
	
	if s.conflicts % 1000 == 0 {
		s.selfSubsumption()
		s.variableElimination()
	}
	
	if s.verbose && initialClauses != s.cnf.NumClauses {
		fmt.Printf("c [verbose] Inprocessing: reduced from %d to %d clauses\n", initialClauses, s.cnf.NumClauses)
	}
}

func (s *CDCLSolver) unitPropagationPreprocess() SolveResult {
	changed := true
	for changed {
		changed = false
		
		clauseCount := len(s.cnf.Clauses)
		for clauseIdx := 0; clauseIdx < clauseCount; clauseIdx++ {
			clause := s.cnf.Clauses[clauseIdx]
			
			satisfied := false
			falseCount := 0
			unassignedCount := 0
			var unassignedLit cnf.Literal
			
			for _, lit := range clause.Literals {
				varIdx := lit.Var()
				if s.assignments[varIdx].Level != 0 {
					assign := s.assignments[varIdx]
					isTrue := (!lit.IsNegated() && assign.Value) || (lit.IsNegated() && !assign.Value)
					if isTrue {
						satisfied = true
						break
					}
					falseCount++
				} else {
					unassignedCount++
					unassignedLit = lit
				}
			}
			
			if satisfied {
				continue
			}
			
			if unassignedCount == 0 && falseCount > 0 {
				if s.verbose {
					fmt.Printf("c [verbose] Preprocessing: conflict in unit propagation\n")
				}
				return UNSAT
			}
			
			if unassignedCount == 1 && falseCount == len(clause.Literals)-1 {
				varIdx := unassignedLit.Var()
				value := !unassignedLit.IsNegated()
				s.assignments[varIdx] = Assignment{
					Value: value,
					Level: 1,
				}
				changed = true
				
				conflict := s.simplifyAfterAssignment(varIdx, value)
				if conflict {
					if s.verbose {
						fmt.Printf("c [verbose] Preprocessing: empty clause created\n")
					}
					return UNSAT
				}
				
				clauseCount = len(s.cnf.Clauses)
				if clauseIdx >= clauseCount {
					clauseIdx = clauseCount - 1
				}
			}
		}
	}
	
	return UNKNOWN
}

func (s *CDCLSolver) simplifyAfterAssignment(varIdx uint32, value bool) bool {
	newClauses := make([]cnf.Clause, 0, len(s.cnf.Clauses))
	for _, clause := range s.cnf.Clauses {
		satisfied := false
		newLiterals := make([]cnf.Literal, 0, len(clause.Literals))
		
		for _, lit := range clause.Literals {
			if lit.Var() == varIdx {
				litValue := !lit.IsNegated()
				if litValue == value {
					satisfied = true
					break
				}
			} else {
				newLiterals = append(newLiterals, lit)
			}
		}
		
		if satisfied {
			continue
		}
		
		if len(newLiterals) == 0 {
			return true
		}
		
		newClauses = append(newClauses, cnf.Clause{Literals: newLiterals, Learned: false})
	}
	s.cnf.Clauses = newClauses
	s.cnf.NumClauses = len(newClauses)
	return false
}

func (s *CDCLSolver) equivalenceDetection() SolveResult {
	// Detect equivalence relations from binary clauses
	// Pattern: (¬a ∨ b) ∧ (¬b ∨ a) means a ↔ b
	// Build equivalence classes and substitute representatives
	
	// Step 1: Find all binary equivalence clauses
	// Store as adjacency list: equivGraph[a] = list of variables equivalent to a
	equivGraph := make(map[uint32][]uint32)
	
	for _, clause := range s.cnf.Clauses {
		if len(clause.Literals) != 2 {
			continue
		}
		
		lit1 := clause.Literals[0]
		lit2 := clause.Literals[1]
		
		// Check for (¬a ∨ b) pattern
		// This is equivalent to: a → b
		var a, b uint32
		var aNeg, bNeg bool
		
		if lit1.IsNegated() && !lit2.IsNegated() {
			// (¬a ∨ b): a = lit1.Var(), b = lit2.Var()
			a = lit1.Var()
			b = lit2.Var()
			aNeg = true
			bNeg = false
		} else if !lit1.IsNegated() && lit2.IsNegated() {
			// (a ∨ ¬b): a = lit1.Var(), b = lit2.Var()
			a = lit1.Var()
			b = lit2.Var()
			aNeg = false
			bNeg = true
		} else {
			continue // Not an implication pattern
		}
		
		// Store directed implication: a → b (with polarity info)
		// We need both (¬a ∨ b) AND (¬b ∨ a) for equivalence
		if aNeg && !bNeg {
			// This is (¬a ∨ b) = a → b
			if _, exists := equivGraph[a]; !exists {
				equivGraph[a] = make([]uint32, 0)
			}
			// Mark that a implies b (we'll check for b implies a later)
			equivGraph[a] = append(equivGraph[a], b)
		}
	}
	
	// Step 2: Find bidirectional implications (equivalences)
	// Use union-find to group equivalent variables
	parent := make([]uint32, s.cnf.NumVars)
	for i := range parent {
		parent[i] = uint32(i)
	}
	
	var find func(uint32) uint32
	find = func(x uint32) uint32 {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	
	union := func(x, y uint32) {
		px, py := find(x), find(y)
		if px != py {
			parent[px] = py
		}
	}
	
	// Check for bidirectional implications
	for a, implications := range equivGraph {
		for _, b := range implications {
			// Check if b also implies a
			if bImps, exists := equivGraph[b]; exists {
				for _, c := range bImps {
					if c == a {
						// Found: a → b and b → a, so a ↔ b
						union(a, b)
					}
				}
			}
		}
	}
	
	// Step 3: Count equivalence classes and substitutions
	equivCount := 0
	substituted := make([]bool, s.cnf.NumVars)
	
	// For each equivalence class, pick representative (lowest var index)
	// Substitute all other variables with representative
	classMembers := make(map[uint32][]uint32)
	for varIdx := uint32(0); varIdx < s.cnf.NumVars; varIdx++ {
		root := find(varIdx)
		classMembers[root] = append(classMembers[root], uint32(varIdx))
	}
	
	// Build substitution map: varIdx -> (representative, samePolarity)
	type substitution struct {
		rep    uint32
		samePol bool
	}
	substMap := make(map[uint32]substitution)
	
	for _, members := range classMembers {
		if len(members) < 2 {
			continue
		}
		
		// Representative is the lowest index
		rep := members[0]
		for _, m := range members[1:] {
			substMap[m] = substitution{rep: rep, samePol: true}
			substituted[m] = true
			equivCount++
		}
	}
	
	if equivCount == 0 {
		if s.verbose {
			fmt.Printf("c [verbose] Equivalence detection: no equivalences found\n")
		}
		return UNKNOWN
	}
	
	// Step 4: Substitute throughout formula
	newClauses := make([]cnf.Clause, 0, len(s.cnf.Clauses))
	
	for _, clause := range s.cnf.Clauses {
		newLiterals := make([]cnf.Literal, 0, len(clause.Literals))
		clauseChanged := false
		
		for _, lit := range clause.Literals {
			varIdx := lit.Var()
			
			if subst, exists := substMap[varIdx]; exists {
				// Substitute with representative
				newLit := cnf.NewLiteral(subst.rep, lit.IsNegated() != subst.samePol)
				newLiterals = append(newLiterals, newLit)
				clauseChanged = true
			} else {
				newLiterals = append(newLiterals, lit)
			}
		}
		
		// Remove duplicate literals after substitution
		if clauseChanged {
			seen := make(map[uint32]bool)
			uniqueLiterals := make([]cnf.Literal, 0)
			hasBothPolarities := false
			
			for _, lit := range newLiterals {
				varIdx := lit.Var()
				if _, exists := seen[varIdx]; exists {
					// Check if we already have opposite polarity
					existingLit := cnf.Literal(varIdx)
					if existingLit.IsNegated() != lit.IsNegated() {
						// Both polarities present → clause is tautology
						hasBothPolarities = true
						break
					}
				} else {
					seen[varIdx] = lit.IsNegated()
					uniqueLiterals = append(uniqueLiterals, lit)
				}
			}
			
			if hasBothPolarities {
				continue // Tautology, skip
			}
			newLiterals = uniqueLiterals
		}
		
		if len(newLiterals) == 0 {
			// Empty clause → UNSAT
			if s.verbose {
				fmt.Printf("c [verbose] Equivalence detection: empty clause created (UNSAT)\n")
			}
			return UNSAT
		}
		
		newClauses = append(newClauses, cnf.Clause{Literals: newLiterals, Learned: clause.Learned})
	}
	
	s.cnf.Clauses = newClauses
	s.cnf.NumClauses = len(newClauses)
	
	// Update VSIDS for eliminated variables
	for varIdx := range substMap {
		// Discourage eliminated vars by setting very low activity
		s.vsids.activity[varIdx] = 0.0
	}
	
	if s.verbose {
		fmt.Printf("c [verbose] Equivalence detection: found %d equivalences, substituted %d variables\n", 
			equivCount, len(substMap))
	}
	
	return UNKNOWN
}

func (s *CDCLSolver) pureLiteralElimination() SolveResult {
	changed := true
	for changed {
		changed = false
		
		hasPositive := make([]bool, s.cnf.NumVars)
		hasNegative := make([]bool, s.cnf.NumVars)
		
		for _, clause := range s.cnf.Clauses {
			for _, lit := range clause.Literals {
				varIdx := lit.Var()
				if s.assignments[varIdx].Level != 0 {
					continue
				}
				if lit.IsNegated() {
					hasNegative[varIdx] = true
				} else {
					hasPositive[varIdx] = true
				}
			}
		}
		
		for varIdx := uint32(0); varIdx < s.cnf.NumVars; varIdx++ {
			if s.assignments[varIdx].Level != 0 {
				continue
			}
			
			isPure := false
			pureValue := false
			
			if hasPositive[varIdx] && !hasNegative[varIdx] {
				isPure = true
				pureValue = true
			} else if hasNegative[varIdx] && !hasPositive[varIdx] {
				isPure = true
				pureValue = false
			}
			
			if isPure {
				s.assignments[varIdx] = Assignment{
					Value: pureValue,
					Level: 1,
				}
				changed = true
				
				conflict := s.simplifyAfterAssignment(varIdx, pureValue)
				if conflict {
					if s.verbose {
						fmt.Printf("c [verbose] Pure literal elimination: empty clause created\n")
					}
					return UNSAT
				}
				
				if s.verbose {
					fmt.Printf("c [verbose] Pure literal elimination: assigned var %d = %v\n", varIdx, pureValue)
				}
			}
		}
	}
	
	if len(s.cnf.Clauses) == 0 {
		if s.verbose {
			fmt.Printf("c [verbose] Pure literal elimination: all clauses satisfied\n")
		}
		return SAT
	}
	
	return UNKNOWN
}

func (s *CDCLSolver) Solve() bool {
	result := s.SolveWithResult()
	return result == SAT
}

func (s *CDCLSolver) SolveWithResult() SolveResult {
	preprocessResult := s.preprocessAggressive()
	if preprocessResult != UNKNOWN {
		if s.verbose {
			s.printStats()
		}
		return preprocessResult
	}

	for {
		s.iterations++
		if s.maxIter > 0 && s.iterations > s.maxIter {
			if s.verbose {
				fmt.Printf("c [verbose] Iteration limit reached (%d)\n", s.maxIter)
				s.printStats()
			}
			return UNKNOWN
		}
		
		conflict, clauseIdx := s.propagate()
		if conflict {
			s.handleConflict(clauseIdx)
			if s.conflicts % 50 == 0 && s.verbose {
				fmt.Printf("c [verbose] Conflict %d, level %d, learned %d\n", s.conflicts, s.level, len(s.learnedClauses))
			}
			if !s.backtrack() {
				if s.verbose {
					s.printStats()
				}
				return UNSAT
			}
			s.backjumpLevel = 0
			
			if s.shouldRestart() {
				s.restart()
			}
			continue
		}

		if s.allAssigned() {
			if s.verbose {
				s.printStats()
			}
			return SAT
		}

		if !s.decide() {
			if s.verbose {
				s.printStats()
			}
			return UNSAT
		}
	}
}

// GetAssignments returns the current assignments for model extraction
func (s *CDCLSolver) GetAssignments() []Assignment {
	return s.assignments
}

func (s *CDCLSolver) allAssigned() bool {
	for i := uint32(0); i < s.cnf.NumVars; i++ {
		if s.assignments[i].Level == 0 {
			return false
		}
	}
	return true
}

func (s *CDCLSolver) propagate() (bool, int) {
	trailIndex := s.trailHead[s.level]

	// For initial unit propagation at level 0, we need to check all clauses even with empty trail
	// Use a do-while pattern: always check at least once per level
	firstPass := true
	for firstPass || trailIndex < len(s.trail) {
		firstPass = false
		unitPropagated := false
		
		// Check original clauses for conflicts and unit propagation
		for clauseIdx := range s.cnf.Clauses {
			clause := &s.cnf.Clauses[clauseIdx]
			
			// Count satisfied, false, and unassigned literals
			satisfiedCount := 0
			falseCount := 0
			unassignedCount := 0
			var unassignedLit cnf.Literal
			
			for _, lit := range clause.Literals {
				varIdx := lit.Var()
				litLevel := s.assignments[varIdx].Level
				if litLevel == 0 {
					unassignedCount++
					unassignedLit = lit
				} else {
					// Inline literalIsTrue check for performance
					assign := s.assignments[varIdx]
					isTrue := (!lit.IsNegated() && assign.Value) || (lit.IsNegated() && !assign.Value)
					if isTrue {
						satisfiedCount++
					} else {
						falseCount++
					}
				}
			}
			
			if satisfiedCount > 0 {
				continue // Clause is satisfied
			}
			
			if unassignedCount == 0 && falseCount > 0 {
				// All literals are false - conflict!
				return true, clauseIdx
			}
			
			if unassignedCount == 1 && falseCount == len(clause.Literals)-1 {
				// Unit clause - propagate the unassigned literal
				// Use max(1, s.level) to ensure we never assign at level 0
				assignLevel := s.level
				if assignLevel == 0 {
					assignLevel = 1
				}
				s.assignLiteral(unassignedLit, assignLevel, clauseIdx)
				unitPropagated = true
				break
			}
		}
		
		// If we propagated a unit, restart from beginning
		if unitPropagated {
			trailIndex = s.trailHead[s.level]
			continue
		}
		
		// Check learned clauses for conflicts and unit propagation
		for learnedIdx := range s.learnedClauses {
			clause := &s.learnedClauses[learnedIdx]
			
			// Count satisfied, false, and unassigned literals
			satisfiedCount := 0
			falseCount := 0
			unassignedCount := 0
			var unassignedLit cnf.Literal
			
			for _, lit := range clause.Literals {
				varIdx := lit.Var()
				litLevel := s.assignments[varIdx].Level
				if litLevel == 0 {
					unassignedCount++
					unassignedLit = lit
				} else {
					// Inline literalIsTrue check for performance
					assign := s.assignments[varIdx]
					isTrue := (!lit.IsNegated() && assign.Value) || (lit.IsNegated() && !assign.Value)
					if isTrue {
						satisfiedCount++
					} else {
						falseCount++
					}
				}
			}
			
			if satisfiedCount > 0 {
				continue // Clause is satisfied
			}
			
			if unassignedCount == 0 && falseCount > 0 {
				// All literals are false - conflict!
				return true, -learnedIdx - 1 // negative to distinguish from original clauses
			}
			
			if unassignedCount == 1 && falseCount == len(clause.Literals)-1 {
				// Unit clause - propagate the unassigned literal
				s.assignLiteral(unassignedLit, s.level, -learnedIdx-1)
				unitPropagated = true
				break
			}
		}
		
		// If we propagated a unit from learned clause, restart from beginning
		if unitPropagated {
			trailIndex = s.trailHead[s.level]
			continue
		}
		
		trailIndex++
	}

	return false, -1
}

// selectRandomUnassigned selects a random unassigned variable
func (s *CDCLSolver) selectRandomUnassigned() uint32 {
	unassigned := make([]uint32, 0)
	for i := uint32(0); i < s.cnf.NumVars; i++ {
		if s.assignments[i].Level == 0 {
			unassigned = append(unassigned, i)
		}
	}
	
	if len(unassigned) == 0 {
		return 0
	}
	
	// Simple deterministic "random" selection based on conflict count
	// This ensures reproducibility while providing diversification
	idx := s.conflicts % len(unassigned)
	return unassigned[idx]
}

func (s *CDCLSolver) decide() bool {
	if !s.vsids.hasUnassigned(s.assignments, s.cnf.NumVars) {
		return false
	}

	// Track conflicts at current level
	if s.level > 0 && s.level < len(s.conflictsAtLevel) {
		s.conflictsAtLevel[s.level]++
	}
	
	// Diversification: force random decision if stuck at same level
	// This helps escape local minima in the search space
	stuckThreshold := 500 // conflicts at same level before forcing random
	forceRandom := false
	
	if s.level > 0 && s.conflictsAtLevel[s.level] > stuckThreshold {
		// Been stuck at this level for too long
		if s.conflicts - s.lastRandomDecision > 100 { // At least 100 conflicts since last random
			forceRandom = true
		}
	}
	
	// Also add 5% random decisions to prevent getting stuck
	if !forceRandom && s.conflicts > 0 && s.conflicts % 20 == 0 {
		// 5% chance of random decision
		forceRandom = true
	}
	
	var varIdx uint32
	var phase bool
	
	if forceRandom {
		// Select random unassigned variable
		varIdx = s.selectRandomUnassigned()
		// Random phase
		phase = s.conflicts % 2 == 0
		s.lastRandomDecision = s.conflicts
		
		if s.verbose && s.conflicts % 1000 == 0 {
			fmt.Printf("c [verbose] Diversification: random decision at conflict %d, level %d\n", s.conflicts, s.level)
		}
		
		// Reset conflicts at this level after random decision
		if s.level > 0 && s.level < len(s.conflictsAtLevel) {
			s.conflictsAtLevel[s.level] = 0
		}
	} else {
		varIdx, phase = s.vsids.selectVariableWithPhase(s.assignments, s.savedPhase)
	}

	s.level++
	s.trailHead = append(s.trailHead, len(s.trail))
	s.assignLiteral(cnf.NewLiteral(varIdx, phase), s.level, -1)
	s.decisions++
	return true
}

func (s *CDCLSolver) assignLiteral(lit cnf.Literal, level int, clauseIdx int) {
	varIdx := lit.Var()

	if s.assignments[varIdx].Level != 0 {
		return
	}

	value := !lit.IsNegated()
	s.assignments[varIdx] = Assignment{
		Value: value,
		Level: level,
	}
	s.trail = append(s.trail, int(varIdx))
	s.implication[varIdx] = clauseIdx
	
	// Save the phase (polarity) that satisfied this variable
	s.savedPhase[varIdx] = value
}

func (s *CDCLSolver) literalIsTrue(lit cnf.Literal) bool {
	assign := s.assignments[lit.Var()]
	if lit.IsNegated() {
		return !assign.Value
	}
	return assign.Value
}

func (s *CDCLSolver) handleConflict(clauseIdx int) {
	s.conflicts++
	
	// Get the conflicting clause
	var conflictLits []cnf.Literal
	if clauseIdx >= 0 {
		conflictLits = s.cnf.Clauses[clauseIdx].Literals
	} else {
		// Learned clause (encoded as negative index)
		learnedIdx := -clauseIdx - 1
		conflictLits = s.learnedClauses[learnedIdx].Literals
		// Bump activity for learned clause involved in conflict
		if learnedIdx < len(s.clauseActivity) {
			s.clauseActivity[learnedIdx] += 1.0
		}
	}
	
	s.vsids.bumpClause(conflictLits)
	
	// Learn clause using 1-UIP analysis and get backjump level
	bjLevel := s.learnClause(conflictLits)
	s.backjumpLevel = bjLevel

	// Decay clause activity periodically
	if s.conflicts%100 == 0 {
		s.vsids.decay()
		// Also decay clause activity
		for i := range s.clauseActivity {
			s.clauseActivity[i] *= 0.95
		}
	}
}

func (s *CDCLSolver) learnClause(conflictLits []cnf.Literal) int {
	// 1-UIP clause learning
	// Start with the conflicting clause and resolve with reason clauses
	// until we have exactly one literal at the current decision level
	
	// Track which literals are in the learned clause
	literalInClause := make([]bool, s.cnf.NumVars)
	literalIsNegated := make([]bool, s.cnf.NumVars)
	
	// Count literals at each level
	levelCount := make([]int, s.level+1)
	
	// Add all literals from the conflicting clause
	for _, lit := range conflictLits {
		varIdx := lit.Var()
		if !literalInClause[varIdx] {
			literalInClause[varIdx] = true
			literalIsNegated[varIdx] = lit.IsNegated()
			lvl := s.assignments[varIdx].Level
			if lvl <= s.level {
				levelCount[lvl]++
			}
		}
	}
	
	// Resolve with reason clauses for literals at current level
	// Work backwards through the trail at current level
	for i := len(s.trail) - 1; i >= s.trailHead[s.level] && levelCount[s.level] > 1; i-- {
		varIdx := uint32(s.trail[i])
		
		if !literalInClause[varIdx] {
			continue // This variable is not in our learned clause
		}
		
		// Get the reason clause for this literal
		reasonIdx := s.implication[varIdx]
		if reasonIdx < 0 {
			continue // Decision variable, no reason clause
		}
		
		// Get the reason clause literals
		var reasonLits []cnf.Literal
		if reasonIdx >= 0 {
			reasonLits = s.cnf.Clauses[reasonIdx].Literals
		} else {
			learnedIdx := -reasonIdx - 1
			reasonLits = s.learnedClauses[learnedIdx].Literals
		}
		
		// Remove this literal from the learned clause (resolution)
		literalInClause[varIdx] = false
		levelCount[s.assignments[varIdx].Level]--
		
		// Add all other literals from the reason clause
		for _, lit := range reasonLits {
			v := lit.Var()
			if v == varIdx {
				continue // Skip the literal we're resolving on
			}
			if !literalInClause[v] {
				literalInClause[v] = true
				literalIsNegated[v] = lit.IsNegated()
				lvl := s.assignments[v].Level
				if lvl <= s.level {
					levelCount[lvl]++
				}
			}
		}
	}
	
	// Build the learned clause from remaining literals
	learnedLits := make([]cnf.Literal, 0)
	for varIdx, inClause := range literalInClause {
		if inClause {
			learnedLits = append(learnedLits, cnf.NewLiteral(uint32(varIdx), literalIsNegated[varIdx]))
		}
	}
	
		// Only learn non-empty clauses
		if len(learnedLits) > 0 {
			// Check if we need to delete clauses
			if len(s.learnedClauses) >= s.maxLearned {
				if s.verbose {
					fmt.Printf("c [verbose] Triggering deletion: %d clauses >= maxLearned %d\n", len(s.learnedClauses), s.maxLearned)
				}
				s.deleteLearnedClauses()
			}
		
		// Allocate in arena (contiguous memory)
		_ = s.learnedArena.AllocateClause(learnedLits, true)
		
		// Track metadata in parallel arrays
		s.clauseActivity = append(s.clauseActivity, 0.0)
		s.clauseAge = append(s.clauseAge, s.currentAge)
		s.clauseSize = append(s.clauseSize, len(learnedLits))
		s.currentAge++
		
		// Also keep in slice for compatibility (temporary)
		newClause := cnf.Clause{Literals: learnedLits, Learned: true}
		s.learnedClauses = append(s.learnedClauses, newClause)
	}
	
	// Calculate backjump level: second-highest level in learned clause
	// The 1-UIP clause has exactly one literal at current level
	// Backjump to the highest level among the other literals
	backjumpLevel := 0
	for varIdx, inClause := range literalInClause {
		if inClause {
			lvl := s.assignments[varIdx].Level
			if lvl > backjumpLevel && lvl < s.level {
				backjumpLevel = lvl
			}
		}
	}
	
	// If no other level found, backjump to level 0 (but we'll use level 1 minimum)
	if backjumpLevel == 0 {
		backjumpLevel = 1
	}
	
	// Calculate LBD for adaptive restarts
	levelSet := make(map[int]bool)
	for varIdx, inClause := range literalInClause {
		if inClause {
			lvl := s.assignments[varIdx].Level
			if lvl > 0 {
				levelSet[lvl] = true
			}
		}
	}
	lbd := len(levelSet)
	
	// Update LBD statistics for adaptive restarts
	s.lastConflictLBD = lbd
	s.lbdSum += lbd
	s.lbdCount++
	
	return backjumpLevel
}

func (s *CDCLSolver) deleteLearnedClauses() {
	// LBD + Size + Activity based clause deletion
	// Protect: Glue clauses (LBD ≤ 3), short clauses (< 10 literals), active clauses
	// Delete: Large clauses (> 20 literals), high LBD (> 6), old inactive clauses
	
	type clauseInfo struct {
		idx      int
		lbd      int
		size     int
		age      int
		activity float64
		score    float64 // Higher = more likely to delete
	}
	
	clauses := make([]clauseInfo, 0, len(s.learnedClauses))
	
	for i, clause := range s.learnedClauses {
		// Calculate LBD
		levelSet := make(map[int]bool)
		for _, lit := range clause.Literals {
			lvl := s.assignments[lit.Var()].Level
			if lvl > 0 {
				levelSet[lvl] = true
			}
		}
		lbd := len(levelSet)
		
		size := len(clause.Literals)
		age := s.currentAge - s.clauseAge[i]
		activity := s.clauseActivity[i]
		
		// Calculate deletion score (higher = delete first)
		// Base score from LBD (most important)
		score := float64(lbd) * 100.0
		
		// Penalty for large size (aggressively delete long clauses)
		if size > 20 {
			score += float64(size-20) * 50.0
		}
		
		// Penalty for old age
		score += float64(age) * 0.5
		
		// Bonus for activity (reduce score for active clauses)
		score -= activity * 10.0
		
		// PROTECTION: Never delete glue clauses with LBD <= 5
		if lbd <= 5 {
			score = -1000.0 // Very low score = never delete
		}
		
		// PROTECTION: Never delete very short clauses (< 5 literals)
		if size < 5 {
			score = -500.0 // Very low score
		}
		
		clauses = append(clauses, clauseInfo{
			idx:      i,
			lbd:      lbd,
			size:     size,
			age:      age,
			activity: activity,
			score:    score,
		})
	}
	
	// Sort by score (descending - highest score = delete first)
	for i := 0; i < len(clauses); i++ {
		for j := i + 1; j < len(clauses); j++ {
			if clauses[i].score < clauses[j].score {
				clauses[i], clauses[j] = clauses[j], clauses[i]
			}
		}
	}
	
	// Target: reduce to minLearned clauses (aggressive deletion)
	toKeep := s.minLearned
	if toKeep > len(s.learnedClauses) {
		toKeep = len(s.learnedClauses) // Can't keep more than we have
	}
	toDelete := len(s.learnedClauses) - toKeep
	
	// Mark clauses to delete
	keep := make([]bool, len(s.learnedClauses))
	deleted := 0
	
	for i := range keep {
		keep[i] = true // Default: keep all
	}
	
	for i := 0; i < len(clauses) && deleted < toDelete; i++ {
		idx := clauses[i].idx
		// Skip protected clauses (score < 0 means protected)
		if clauses[i].score < 0 {
			continue
		}
		keep[idx] = false
		deleted++
	}
	
	// Compact the slices
	newClauses := make([]cnf.Clause, 0, toKeep)
	newActivity := make([]float64, 0, toKeep)
	newAge := make([]int, 0, toKeep)
	newSize := make([]int, 0, toKeep)
	
	for i := range s.learnedClauses {
		if keep[i] {
			newClauses = append(newClauses, s.learnedClauses[i])
			newActivity = append(newActivity, s.clauseActivity[i])
			newAge = append(newAge, s.clauseAge[i])
			newSize = append(newSize, s.clauseSize[i])
		}
	}
	
	s.learnedClauses = newClauses
	s.clauseActivity = newActivity
	s.clauseAge = newAge
	s.clauseSize = newSize
	
	// Rebuild arena from compacted slices (ensures contiguous memory)
	s.learnedArena.Reset()
	for i, clause := range s.learnedClauses {
		s.learnedArena.AllocateClause(clause.Literals, true)
		_ = i // Use index variable
	}
	
	if s.verbose {
		fmt.Printf("c [verbose] Deleted %d learned clauses, kept %d (target: %d)\n", deleted, len(s.learnedClauses), toKeep)
	}
}

func (s *CDCLSolver) backtrack() bool {
	if len(s.trailHead) <= 1 {
		return false
	}

	// Use backjump level if available, otherwise backtrack one level
	bjLevel := s.backjumpLevel
	if bjLevel <= 0 || bjLevel >= s.level {
		bjLevel = s.level - 1
	}
	if bjLevel < 1 {
		return false
	}
	
	// Find the decision point at the backjump level
	decisionPoint := s.trailHead[bjLevel]
	if decisionPoint >= len(s.trail) {
		return false
	}
	
	decisionVar := uint32(s.trail[decisionPoint])
	decisionValue := s.assignments[decisionVar].Value

	// Clear all assignments from decisionPoint onwards
	for i := decisionPoint; i < len(s.trail); i++ {
		varIdx := uint32(s.trail[i])
		s.assignments[varIdx] = Assignment{}
		s.implication[varIdx] = -1
	}
	s.trail = s.trail[:decisionPoint]
	s.trailHead = s.trailHead[:bjLevel+1]
	s.level = bjLevel
	
	// Reset conflicts at levels > bjLevel since we're backtracking
	for i := bjLevel + 1; i < len(s.conflictsAtLevel); i++ {
		s.conflictsAtLevel[i] = 0
	}

	// Flip the decision at the backjump level
	s.assignments[decisionVar] = Assignment{
		Value: !decisionValue,
		Level: bjLevel,
	}
	s.trail = append(s.trail, int(decisionVar))
	
	return true
}


