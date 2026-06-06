# Watched Literals Bug Analysis

## Problem

The solver hangs/times out on instances with many 4-literal clauses, even though MiniSat solves them in <0.1 seconds.

Example: `11c893b7c37aeb53cdaf5f677dda0b7d.cnf` (36 vars, 144 clauses, all 4-literal)
- MiniSat: 0.075s, 39,968 conflicts
- Satience: TIMEOUT after 60s

## Root Cause

The `propagateLong()` function has a critical bug in the watched literals implementation that causes watch lists to grow exponentially with duplicate entries.

### Bug Details

When updating watched literals in `propagateLong()`, the code appends clauses to watch lists without checking for duplicates:

```go
// BUGGY CODE (lines ~2163, ~2189)
s.cnf.WatchListLong[litIdx] = append(s.cnf.WatchListLong[litIdx], watchIdx)
```

This causes the same clause to be added to the same watch list hundreds or thousands of times.

### Evidence

Watch list sizes explode during search:
- Initial: max 7 clauses per watch list
- After search: some watch lists have 100,000+ entries
- Example: literal 68 had 1,169,175 clauses in its watch list!

### Attempted Fixes

1. **Check watch pointers before adding**: Doesn't work because watch pointers are updated AFTER adding to watch list
2. **Scan watch list for duplicates**: Works but watch lists still grow because stale entries aren't removed
3. **Skip stale entries during propagation**: Helps but doesn't prevent growth

The fundamental issue is that the watched literals scheme uses "lazy cleanup" - old watch list entries are not removed when watches are updated, they're just ignored during propagation. But we keep adding NEW entries without removing old ones, causing exponential growth.

## Current Status

**Watched literals for binary and ternary clauses**: Working correctly
**Watched literals for long clauses (>3 literals)**: BROKEN - causes exponential watch list growth

## Temporary Workaround

Disabled watched literals for long clauses, using simple linear scanning instead:

```go
// In propagate(), skip propagateLong() and scan all clauses >= 4 literals
```

This is correct but SLOW - the instance still times out because linear scanning of all clauses is O(n) per propagation step.

## Proper Fix Required

To fix this properly, one of these approaches is needed:

### Option 1: Proper Watch List Management
- When updating a watch from literal A to literal B:
  1. Remove clause from A's watch list (O(n) operation)
  2. Add clause to B's watch list
- Use efficient data structures (e.g., doubly-linked lists) for O(1) removal

### Option 2: Lazy Cleanup with Periodic Rebuilding
- Continue using lazy cleanup (don't remove old entries)
- But periodically REBUILD all watch lists from scratch when they grow too large
- Trade-off: rebuilding is O(n) but done infrequently

### Option 3: Use Watch Indices Instead of Scanning
- Store watch list indices in the clause structure
- When updating watches, update the indices directly
- Avoid scanning watch lists during propagation

## Recommendation

Given the complexity and the risk of introducing soundness bugs, I recommend:

1. **Short-term**: Keep watched literals disabled for long clauses (current workaround)
2. **Medium-term**: Implement Option 2 (periodic rebuilding) - simpler and safer
3. **Long-term**: Consider Option 1 with careful testing

## Testing

After fixing, verify with:
```bash
# This instance should solve in <1 second
./satience benchmark/gbd_instances/11c893b7c37aeb53cdaf5f677dda0b7d.cnf

# Compare with MiniSat
time minisat benchmark/gbd_instances/11c893b7c37aeb53cdaf5f677dda0b7d.cnf /tmp/out.txt
```

Expected: Satience within 10x of MiniSat (currently >1000x slower)
