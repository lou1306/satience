package solver

import "satience/internal/cnf"

// ClauseDatabase manages learned clauses with deletion strategy
type ClauseDatabase struct {
	clauses       []cnf.Clause
	activity      []float64 // Activity for each learned clause
	maxClauses    int       // Maximum number of learned clauses to keep
	deleteRatio   float64   // Ratio of clauses to delete when limit reached
}

// NewClauseDatabase creates a new clause database
func NewClauseDatabase(maxClauses int) *ClauseDatabase {
	return &ClauseDatabase{
		clauses:      make([]cnf.Clause, 0),
		activity:     make([]float64, 0),
		maxClauses:   maxClauses,
		deleteRatio:  0.5,
	}
}

// Add adds a learned clause to the database
func (db *ClauseDatabase) Add(clause cnf.Clause) int {
	db.clauses = append(db.clauses, clause)
	db.activity = append(db.activity, 0.0)
	return len(db.clauses) - 1
}

// Bump increases the activity of a clause
func (db *ClauseDatabase) Bump(clauseIdx int) {
	if clauseIdx >= 0 && clauseIdx < len(db.activity) {
		db.activity[clauseIdx] += 1.0
	}
}

// Decay decays all clause activities
func (db *ClauseDatabase) Decay() {
	for i := range db.activity {
		db.activity[i] *= 0.5
	}
}

// Cleanup removes low-activity clauses when database is too large
// Returns indices of clauses to keep
func (db *ClauseDatabase) Cleanup() []int {
	if len(db.clauses) <= db.maxClauses {
		return nil
	}

	// Calculate how many to delete
	numToDelete := int(float64(len(db.clauses)) * db.deleteRatio)
	if numToDelete < 1 {
		numToDelete = 1
	}

	// Sort by activity and mark low-activity clauses for deletion
	type indexedActivity struct {
		idx      int
		activity float64
	}

	activities := make([]indexedActivity, len(db.activity))
	for i, act := range db.activity {
		activities[i] = indexedActivity{i, act}
	}

	// Simple selection of lowest activity clauses
	keep := make([]bool, len(db.clauses))
	for i := range keep {
		keep[i] = true
	}

	// Mark lowest activity clauses for deletion
	deleted := 0
	for i := 0; i < len(activities) && deleted < numToDelete; i++ {
		// Find minimum
		minIdx := i
		for j := i + 1; j < len(activities); j++ {
			if activities[j].activity < activities[minIdx].activity {
				minIdx = j
			}
		}
		
		// Swap
		activities[i], activities[minIdx] = activities[minIdx], activities[i]
		
		// Mark for deletion (but keep unit and binary clauses)
		clause := &db.clauses[activities[i].idx]
		if len(clause.Literals) > 2 {
			keep[activities[i].idx] = false
			deleted++
		}
	}

	// Compact the database
	newClauses := make([]cnf.Clause, 0)
	newActivity := make([]float64, 0)
	keepIndices := make([]int, 0)

	for i, k := range keep {
		if k {
			keepIndices = append(keepIndices, i)
			newClauses = append(newClauses, db.clauses[i])
			newActivity = append(newActivity, db.activity[i])
		}
	}

	db.clauses = newClauses
	db.activity = newActivity

	return keepIndices
}

// Get returns a clause by index
func (db *ClauseDatabase) Get(idx int) *cnf.Clause {
	if idx < 0 || idx >= len(db.clauses) {
		return nil
	}
	return &db.clauses[idx]
}

// Len returns the number of learned clauses
func (db *ClauseDatabase) Len() int {
	return len(db.clauses)
}

// All returns all learned clauses
func (db *ClauseDatabase) All() []cnf.Clause {
	return db.clauses
}
