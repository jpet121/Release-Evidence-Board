# Release Evidence Board

The board records evidence for a candidate against an approved behavior contract. A sufficient assessment supports a human decision; it grants no deployment authority.

## Language

**Baseline**: An immutable approved roster of behaviors, expected outcomes, tests and policy, identified by its canonical descriptor.

**Candidate**: A release identity bound to execution inputs and an approved baseline, with a visible predecessor when a correction creates a successor.
_Avoid_: Release label as identity

**Run**: A producer-declared execution with a whole-run finish time and planned attempts, including executions with missing or no results.

**Attempt**: One planned test execution. Missing results remain part of the evidence denominator.

**Definitive failure**: Evidence that permanently blocks the same candidate/baseline/behavior identity, including an unreachable pass threshold.

**Assessment**: A rules-derived evaluation at a stated time against a candidate and approved baseline.

**Frozen report**: An immutable, self-contained record of an assessment and its evidence, sufficient to reproduce its result offline.

**Import receipt**: The committed record of an import; an exact retry returns the original receipt.
