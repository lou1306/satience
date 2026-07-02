package parser

import (
	"bufio"
	"fmt"
	"io"
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

	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			// Process the line
			if err := processLine(line, &numVars, &numClauses, &headerFound, &cnfFormula, &currentLits); err != nil {
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

	return cnfFormula, nil
}

// processLine handles a single line of DIMACS CNF input
func processLine(line string, numVars *uint32, numClauses *int, headerFound *bool,
	cnfFormula **cnf.CNF, currentLits *[]cnf.Literal) error {

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
		nc, ni, err := parseUint32From(line, i)
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
		return nil // skip lines before header
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
			// End of clause
			lits := make([]cnf.Literal, len(*currentLits))
			copy(lits, *currentLits)
			(*cnfFormula).AddClause(lits, false)
			*currentLits = (*currentLits)[:0]
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

	val := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		val = val*10 + int(s[i]-'0')
		i++
	}

	if neg {
		val = -val
	}
	return val, i, nil
}

func parseUint32(s string) (uint32, error) {
	val, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(val), nil
}
