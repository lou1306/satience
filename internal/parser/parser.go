package parser

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"

	"satience/internal/cnf"
)

// Parse reads a DIMACS CNF file and returns a CNF formula
func Parse(r io.Reader) (*cnf.CNF, error) {
	scanner := bufio.NewScanner(r)
	
	var numVars uint32
	var numClauses int
	var headerFound bool
	
	// First pass: find header and count clauses
	clauseLines := make([]string, 0)
	
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		
		// Skip empty lines
		if line == "" {
			continue
		}
		
		// Skip comment lines
		if strings.HasPrefix(line, "c") {
			continue
		}
		
		// Parse problem line
		if strings.HasPrefix(line, "p") {
			if headerFound {
				return nil, fmt.Errorf("duplicate problem line")
			}
			
			parts := strings.Fields(line)
			if len(parts) != 4 || parts[1] != "cnf" {
				return nil, fmt.Errorf("invalid problem line: %s", line)
			}
			
			var err error
			numVars, err = parseUint32(parts[2])
			if err != nil {
				return nil, fmt.Errorf("invalid numVars: %v", err)
			}
			
			numClauses, err = strconv.Atoi(parts[3])
			if err != nil {
				return nil, fmt.Errorf("invalid numClauses: %v", err)
			}
			
			headerFound = true
			continue
		}
		
		// After header, collect clause lines
		if headerFound {
			clauseLines = append(clauseLines, line)
		}
	}
	
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading input: %v", err)
	}
	
	if !headerFound {
		return nil, fmt.Errorf("missing problem line (p cnf <vars> <clauses>)")
	}
	
	// Build CNF
	cnfFormula := cnf.NewCNF(numVars, numClauses)
	
	for _, line := range clauseLines {
		clause, err := parseClause(line, numVars)
		if err != nil {
			return nil, err
		}
		cnfFormula.AddClause(clause, false)
	}
	
	return cnfFormula, nil
}

func parseClause(line string, maxVar uint32) ([]cnf.Literal, error) {
	tokens := strings.Fields(line)
	if len(tokens) == 0 {
		return nil, fmt.Errorf("empty clause")
	}
	
	// Last token must be 0
	if tokens[len(tokens)-1] != "0" {
		return nil, fmt.Errorf("clause must end with 0")
	}
	
	literals := make([]cnf.Literal, 0, len(tokens)-1)
	
	for i := 0; i < len(tokens)-1; i++ {
		val, err := strconv.Atoi(tokens[i])
		if err != nil {
			return nil, fmt.Errorf("invalid literal %q: %v", tokens[i], err)
		}
		
		if val == 0 {
			return nil, fmt.Errorf("unexpected 0 in middle of clause")
		}
		
		// Convert from 1-based DIMACS to 0-based internal
		var varIdx uint32
		var negated bool
		
		if val > 0 {
			varIdx = uint32(val) - 1
			negated = false
		} else {
			varIdx = uint32(-val) - 1
			negated = true
		}
		
		if varIdx >= maxVar {
			return nil, fmt.Errorf("variable %d exceeds declared max %d", varIdx+1, maxVar)
		}
		
		literals = append(literals, cnf.NewLiteral(varIdx, negated))
	}
	
	return literals, nil
}

func parseUint32(s string) (uint32, error) {
	val, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(val), nil
}
