package parser

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"strconv"

	"satience/internal/cnf"
)

// Parse reads a DIMACS CNF file and returns a CNF formula
func Parse(r io.Reader) (*cnf.CNF, error) {
	br := bufio.NewReader(r)

	var numVars uint32
	var numClauses int
	var headerFound bool
	var cnfFormula *cnf.CNF

	// Reusable buffer for current clause literals
	var currentLits []cnf.Literal

	// Largest variable index actually referenced by any clause (for the
	// declared-numVars plausibility guard against OOM: a DIMACS header may
	// claim billions of vars with a tiny body, and downstream solver code
	// allocates O(numVars) arrays, so an implausibly large declared count
	// relative to the referenced variables is rejected as malformed).
	var maxVarReferenced uint32

	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			// Process the line
			if err := processLine(line, &numVars, &numClauses, &headerFound, &cnfFormula, &currentLits, &maxVarReferenced); err != nil {
				return nil, err
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("error reading input: %v", err)
		}
	}

	if !headerFound {
		return nil, fmt.Errorf("missing problem line (p cnf <vars> <clauses>)")
	}

	// Unterminated clause: literals accumulated without a closing 0
	if len(currentLits) > 0 {
		return nil, fmt.Errorf("unterminated clause at end of input (missing 0)")
	}

	// Declared-numVars plausibility guard against OOM: the header can claim
	// billions of variables while the body references only a handful; the solver
	// then allocates O(numVars) arrays from a tiny input. Reject when the
	// declared count is implausibly larger than the max variable actually used.
	// Generous slack preserves legitimate DIMACS (where numVars is the max
	// referenced index, sometimes with a few isolated trailing variables).
	const plausibleFloor = 1 << 20 // 1,048,576
	if numVars > plausibleFloor && uint64(numVars) > uint64(maxVarReferenced)+1+plausibleFloor {
		return nil, fmt.Errorf("declared %d variables but only referenced up to %d (implausible count)",
			numVars, maxVarReferenced+1)
	}

	return cnfFormula, nil
}

// processLine handles a single line of DIMACS CNF input
func processLine(line string, numVars *uint32, numClauses *int, headerFound *bool,
	cnfFormula **cnf.CNF, currentLits *[]cnf.Literal, maxVarReferenced *uint32) error {

	// Skip leading whitespace
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t' || line[i] == '\r' || line[i] == '\n') {
		i++
	}
	if i >= len(line) {
		return nil // empty line
	}

	first := line[i]

	// Skip comment lines
	if first == 'c' {
		return nil
	}

	// Problem line
	if first == 'p' {
		if *headerFound {
			return fmt.Errorf("duplicate problem line")
		}

		// Parse "p cnf <vars> <clauses>" without strings.Fields
		// Skip "p"
		i++
		// Skip spaces
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		// Read "cnf"
		if i+3 > len(line) || line[i] != 'c' || line[i+1] != 'n' || line[i+2] != 'f' {
			return fmt.Errorf("invalid problem line: expected 'cnf'")
		}
		i += 3
		// Skip spaces
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		// Read numVars
		nv, ni, err := parseUint32From(line, i)
		if err != nil {
			return fmt.Errorf("invalid numVars: %v", err)
		}
		*numVars = nv
		i = ni
		// Skip spaces
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		// Read numClauses
		nc, _, err := parseUint32From(line, i)
		if err != nil {
			return fmt.Errorf("invalid numClauses: %v", err)
		}
		*numClauses = int(nc)
		*headerFound = true
		*cnfFormula = cnf.NewCNF(*numVars, *numClauses)
		return nil
	}

	// Clause line: parse integers directly
	if !*headerFound {
		return fmt.Errorf("clause data before problem line")
	}

	// Parse integers from the line
	for i < len(line) {
		// Skip whitespace
		for i < len(line) && (line[i] == ' ' || line[i] == '\t' || line[i] == '\r' || line[i] == '\n') {
			i++
		}
		if i >= len(line) {
			break
		}

		// Parse integer
		val, ni, err := parseIntFrom(line, i)
		if err != nil {
			return fmt.Errorf("invalid literal at position %d: %v", i, err)
		}
		i = ni

		if val == 0 {
			// End of clause — transfer ownership of the currentLits slice to
			// AddClause (which stores it in Clauses and copies into literalPool)
			// instead of allocating a fresh copy per clause. Then drop the
			// reference so the next clause appends into a fresh allocation
			// rather than aliasing the stored clause's backing array.
			(*cnfFormula).AddClause(*currentLits, false)
			*currentLits = nil
		} else {
			var varIdx uint32
			var negated bool
			if val > 0 {
				varIdx = uint32(val) - 1
				negated = false
			} else {
				varIdx = uint32(-val) - 1
				negated = true
			}
			if varIdx >= *numVars {
				return fmt.Errorf("variable %d exceeds declared max %d", varIdx+1, *numVars)
			}
			if varIdx > *maxVarReferenced {
				*maxVarReferenced = varIdx
			}
			*currentLits = append(*currentLits, cnf.NewLiteral(varIdx, negated))
		}
	}

	return nil
}

// parseUint32From parses a uint32 from string starting at position i
// Returns the value, the next position, and any error
func parseUint32From(s string, i int) (uint32, int, error) {
	val, err := strconv.ParseUint(s[i:], 10, 32)
	if err != nil {
		// Try to find the end of the number
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		val, err = strconv.ParseUint(s[i:j], 10, 32)
		if err != nil {
			return 0, i, err
		}
		return uint32(val), j, nil
	}
	// Find where ParseUint stopped
	j := i
	if j < len(s) && (s[j] == '+' || s[j] == '-') {
		j++
	}
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	return uint32(val), j, nil
}

// parseIntFrom parses an int from string starting at position i
// Returns the value, the next position, and any error
func parseIntFrom(s string, i int) (int, int, error) {
	neg := false
	if i < len(s) && s[i] == '-' {
		neg = true
		i++
	} else if i < len(s) && s[i] == '+' {
		i++
	}

	if i >= len(s) || s[i] < '0' || s[i] > '9' {
		return 0, i, fmt.Errorf("expected digit")
	}

	// DIMACS literals fit in int32. Rejecting values beyond MaxInt32 prevents
	// uint32 truncation in the caller (e.g. 4294967297 aliasing to variable 1).
	const maxVal = math.MaxInt32
	val := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		d := int(s[i] - '0')
		if val > (maxVal-d)/10 {
			return 0, i, fmt.Errorf("literal out of range")
		}
		val = val*10 + d
		i++
	}

	if neg {
		val = -val
	}
	return val, i, nil
}
