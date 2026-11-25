import Lean

/- A requirement-derived decision model for already validated candidate evidence.
   CSV interpretation, identity, authorization and persistence are outside this model. -/

namespace ReleaseEvidenceOracle

open Lean

structure Slot where
  outcome : String
  mustPass : Bool
  finishedUs : Int
  deriving Inhabited, FromJson, ToJson

structure Run where
  tag : String
  closedUs : Int
  sequence : Nat
  slots : List Slot
  deriving Inhabited, FromJson, ToJson

structure Behavior where
  id : String
  required : Bool
  threshold : Nat
  maxAgeUs : Int
  planned : Nat
  runs : List Run
  deriving Inhabited, FromJson, ToJson

structure Input where
  assessedUs : Int
  behaviors : List Behavior
  deriving Inhabited, FromJson, ToJson

def incomplete (planned : Nat) (r : Run) : Bool :=
  r.slots.length != planned ||
    r.slots.any (fun s => s.outcome != "PASS" && s.outcome != "FAIL")

def prefer (planned : Nat) (a b : Run) : Bool :=
  if a.closedUs != b.closedUs then a.closedUs > b.closedUs
  else if incomplete planned a != incomplete planned b then incomplete planned a
  else a.sequence > b.sequence

def current (planned : Nat) (runs : List Run) : Option Run :=
  runs.foldl (fun best r =>
    match best with
    | none => some r
    | some previous => if prefer planned r previous then some r else best) none

def countOutcome (outcome : String) (r : Run) : Nat :=
  (r.slots.filter (fun slot => slot.outcome == outcome)).length

def maximumPasses (planned : Nat) (r : Run) : Nat :=
  planned - countOutcome "FAIL" r

def mustPassFailure (r : Run) : Bool :=
  r.slots.any (fun s => s.mustPass && s.outcome == "FAIL")

def rateFailure (planned threshold : Nat) (r : Run) : Bool :=
  maximumPasses planned r * 10000 < threshold * planned

def definitive (planned threshold : Nat) (r : Run) : Bool :=
  mustPassFailure r || rateFailure planned threshold r

def oldest (r : Run) : Option Int :=
  match r.slots with
  | [] => none
  | s :: rest => some (rest.foldl (fun time result => min time result.finishedUs) s.finishedUs)

inductive Status where
  | pass | fail | stale | unknown
  deriving DecidableEq, BEq, Inhabited

def Status.label : Status → String
  | .pass => "PASS"
  | .fail => "FAIL"
  | .stale => "STALE"
  | .unknown => "UNKNOWN"

def status (assessedUs : Int) (b : Behavior) : Status :=
  if b.runs.any (definitive b.planned b.threshold) then .fail
  else match current b.planned b.runs with
  | none => .unknown
  | some r =>
    if incomplete b.planned r then .unknown
    else match oldest r with
    | none => .unknown
    | some t => if assessedUs > t + b.maxAgeUs then .stale else .pass

def sufficient (assessedUs : Int) (behaviors : List Behavior) : Bool :=
  behaviors.all (fun b => !b.required || decide (status assessedUs b = .pass))

def candidateLabel (assessedUs : Int) (behaviors : List Behavior) : String :=
  if sufficient assessedUs behaviors then "SUFFICIENT" else "BLOCKED"

structure BehaviorDecision where
  id : String
  status : String
  currentRun : Option String
  currentSequence : Option Nat
  planned : Nat
  passes : Nat
  definitiveRuns : List String
  currentAgeUs : Option Int
  completeExpiryUs : Option Int
  deriving ToJson

structure Decision where
  candidateStatus : String
  behaviors : List BehaviorDecision
  deriving ToJson

def behaviorDecision (atUs : Int) (b : Behavior) : BehaviorDecision :=
  let r := current b.planned b.runs
  let t := r.bind oldest
  { id := b.id
    status := (status atUs b).label
    currentRun := r.map (·.tag)
    currentSequence := r.map (·.sequence)
    planned := b.planned
    passes := (r.map (countOutcome "PASS")).getD 0
    definitiveRuns := (b.runs.filter (definitive b.planned b.threshold)).map (·.tag)
    currentAgeUs := t.map (fun time => max 0 (atUs - time))
    completeExpiryUs := r.bind fun actual =>
      if incomplete b.planned actual then none else t.map (· + b.maxAgeUs) }

def decision (input : Input) : Decision :=
  { candidateStatus := candidateLabel input.assessedUs input.behaviors
    behaviors := input.behaviors.map (behaviorDecision input.assessedUs) }

theorem prefer_incomplete_at_tie (planned : Nat) (a b : Run)
    (times : a.closedUs = b.closedUs)
    (aIncomplete : incomplete planned a = true)
    (bComplete : incomplete planned b = false) :
    prefer planned a b = true ∧ prefer planned b a = false := by
  simp [prefer, times, aIncomplete, bComplete]

theorem current_incomplete_for_both_orders (planned : Nat) (a b : Run)
    (times : a.closedUs = b.closedUs)
    (aIncomplete : incomplete planned a = true)
    (bComplete : incomplete planned b = false) :
    current planned [a, b] = some a ∧ current planned [b, a] = some a := by
  have both := prefer_incomplete_at_tie planned a b times aIncomplete bComplete
  simp [current, both.1, both.2]

theorem sufficient_iff_required_pass (assessedUs : Int) (behaviors : List Behavior) :
    sufficient assessedUs behaviors = true ↔
    ∀ b ∈ behaviors, b.required = true → status assessedUs b = .pass := by
  constructor
  · intro h b member required
    have byBehavior := (List.all_eq_true.mp h) b member
    cases result : status assessedUs b <;> simp [required, result] at byBehavior ⊢
  · intro h
    apply List.all_eq_true.mpr
    intro b member
    cases required : b.required with
    | false => simp [required]
    | true => simp [required, h b member required]

theorem definitive_failure_blocks (assessedUs : Int) (behaviors : List Behavior)
    (b : Behavior) (r : Run)
    (included : b ∈ behaviors) (required : b.required = true) (recorded : r ∈ b.runs)
    (failed : definitive b.planned b.threshold r = true) :
    status assessedUs b = .fail ∧ candidateLabel assessedUs behaviors = "BLOCKED" := by
  have persistent : b.runs.any (definitive b.planned b.threshold) = true :=
    List.any_eq_true.mpr ⟨r, recorded, failed⟩
  have behaviorFails : status assessedUs b = .fail := by simp [status, persistent]
  constructor
  · exact behaviorFails
  · cases hs : sufficient assessedUs behaviors with
    | false => simp [candidateLabel, hs]
    | true =>
      have contradictory := (sufficient_iff_required_pass assessedUs behaviors).mp hs b included required
      rw [behaviorFails] at contradictory
      contradiction

theorem no_rate_failure_at_equal_bound (planned threshold : Nat) (r : Run)
    (equal : maximumPasses planned r * 10000 = threshold * planned) :
    rateFailure planned threshold r = false := by
  simp [rateFailure, equal]

theorem exact_age_boundary_is_fresh (b : Behavior) (r : Run) (oldestUs : Int)
    (noFailure : b.runs.any (definitive b.planned b.threshold) = false)
    (selected : current b.planned b.runs = some r)
    (complete : incomplete b.planned r = false)
    (oldestResult : oldest r = some oldestUs) :
    status (oldestUs + b.maxAgeUs) b = .pass ∧
    status (oldestUs + b.maxAgeUs + 1) b = .stale := by
  have older : oldestUs + b.maxAgeUs + 1 > oldestUs + b.maxAgeUs := by omega
  simp [status, noFailure, selected, complete, oldestResult, older]

/- These predicates inspect returned values of a supplied executable. The
   constructor can only pack a function accompanied by this exact contract. -/
structure ProjectsBehavior (atUs : Int) (b : Behavior) (out : BehaviorDecision) : Prop where
  identity : out.id = b.id
  primary : out.status = (status atUs b).label
  selectedTag : out.currentRun = (current b.planned b.runs).map Run.tag
  selectedSequence : out.currentSequence = (current b.planned b.runs).map Run.sequence
  plannedCount : out.planned = b.planned
  passingCount : out.passes = ((current b.planned b.runs).map (countOutcome "PASS")).getD 0
  persistentFailures : out.definitiveRuns = (b.runs.filter (definitive b.planned b.threshold)).map Run.tag
  age : out.currentAgeUs = ((current b.planned b.runs).bind oldest).map (fun t => max 0 (atUs - t))
  expiry : out.completeExpiryUs = (current b.planned b.runs).bind (fun r =>
    if incomplete b.planned r then none else (oldest r).map (· + b.maxAgeUs))

def observeBehavior (run : Input → Decision) (atUs : Int) (b : Behavior) : Option BehaviorDecision :=
  (run { assessedUs := atUs, behaviors := [b] }).behaviors[0]?

def observeCurrent (run : Input → Decision) (atUs : Int) (b : Behavior) : Option (String × Nat) :=
  (observeBehavior run atUs b).bind fun output => do
    let tag ← output.currentRun
    let sequence ← output.currentSequence
    return (tag, sequence)

structure DecisionContract (run : Input → Decision) : Prop where
  shape : ∀ i, (run i).behaviors.length = i.behaviors.length
  orderedProjection : ∀ (i : Input) (index : Nat) (b : Behavior), i.behaviors[index]? = some b →
    ∃ output, (run i).behaviors[index]? = some output ∧ ProjectsBehavior i.assessedUs b output
  requiredPass : ∀ i, (run i).candidateStatus = "SUFFICIENT" ↔
    ∀ b ∈ i.behaviors, b.required = true → status i.assessedUs b = .pass
  retainedFailure : ∀ (i : Input) (index : Nat) (b : Behavior) (r : Run),
    i.behaviors[index]? = some b → b.required = true → r ∈ b.runs →
    definitive b.planned b.threshold r = true →
    (run i).candidateStatus = "BLOCKED" ∧
      ((run i).behaviors[index]?).map BehaviorDecision.status = some "FAIL"
  incompleteTieBothOrders : ∀ (atUs : Int) (b : Behavior) (a c : Run),
    a.closedUs = c.closedUs → incomplete b.planned a = true → incomplete b.planned c = false →
    observeCurrent run atUs { b with runs := [a, c] } = some (a.tag, a.sequence) ∧
      observeCurrent run atUs { b with runs := [c, a] } = some (a.tag, a.sequence)
  exactRateEquality : ∀ (atUs : Int) (b : Behavior) (r : Run),
    maximumPasses b.planned r * 10000 = b.threshold * b.planned → mustPassFailure r = false →
    (observeBehavior run atUs { b with runs := [r] }).map BehaviorDecision.definitiveRuns = some []
  freshnessBoundary : ∀ (b : Behavior) (r : Run) (oldestUs : Int),
    b.runs.any (definitive b.planned b.threshold) = false → current b.planned b.runs = some r →
    incomplete b.planned r = false → oldest r = some oldestUs →
    (observeBehavior run (oldestUs + b.maxAgeUs) b).map BehaviorDecision.status = some "PASS" ∧
      (observeBehavior run (oldestUs + b.maxAgeUs + 1) b).map BehaviorDecision.status = some "STALE"

theorem decision_kernel_refinement : DecisionContract decision where
  shape i := by simp [decision]
  orderedProjection i index b atIndex := by
    refine ⟨behaviorDecision i.assessedUs b, ?_, ?_⟩
    · simp [decision, atIndex]
    · constructor <;> try rfl
      cases chosen : current b.planned b.runs <;> simp [behaviorDecision, chosen]
  requiredPass i := by
    constructor
    · intro actual
      have eligible : sufficient i.assessedUs i.behaviors = true := by
        cases observed : sufficient i.assessedUs i.behaviors with
        | false => simp [decision, candidateLabel, observed] at actual
        | true => rfl
      exact (sufficient_iff_required_pass i.assessedUs i.behaviors).mp eligible
    · intro requiredPass
      have eligible := (sufficient_iff_required_pass i.assessedUs i.behaviors).mpr requiredPass
      simp [decision, candidateLabel, eligible]
  retainedFailure i index b r atIndex required recorded failed := by
    have actual := definitive_failure_blocks i.assessedUs i.behaviors b r
      (List.mem_of_getElem? atIndex) required recorded failed
    exact ⟨actual.2, by simp [decision, atIndex, behaviorDecision, actual.1, Status.label]⟩
  incompleteTieBothOrders atUs b a c sameClose aIncomplete cComplete := by
    have which := current_incomplete_for_both_orders b.planned a c sameClose aIncomplete cComplete
    simp [observeCurrent, observeBehavior, decision, behaviorDecision, which.1, which.2]
  exactRateEquality atUs b r equal noMustPass := by
    have attainable := no_rate_failure_at_equal_bound b.planned b.threshold r equal
    simp [observeBehavior, decision, behaviorDecision, definitive, attainable, noMustPass]
  freshnessBoundary b r oldestUs noFailure selected complete hasOldest := by
    have boundary := exact_age_boundary_is_fresh b r oldestUs noFailure selected complete hasOldest
    simp [observeBehavior, decision, behaviorDecision, boundary.1, boundary.2, Status.label]

def checkedDecision : { run : Input → Decision // DecisionContract run } :=
  ⟨decision, decision_kernel_refinement⟩

theorem executable_has_decision_contract : DecisionContract checkedDecision.val :=
  checkedDecision.property

structure GeneratedCase where
  id : String
  family : String
  input : Input
  deriving FromJson

structure GeneratedInputs where
  schema : String
  seed : String
  cases : Array GeneratedCase
  deriving FromJson

def decodeFile (α : Type) [FromJson α] (path : String) : IO α := do
  let raw ← IO.FS.readFile path
  match Json.parse raw >>= fromJson? with
  | .error error => throw (IO.userError s!"{path}: {error}")
  | .ok value => return value

#print axioms prefer_incomplete_at_tie
#print axioms current_incomplete_for_both_orders
#print axioms sufficient_iff_required_pass
#print axioms definitive_failure_blocks
#print axioms no_rate_failure_at_equal_bound
#print axioms exact_age_boundary_is_fresh
#print axioms decision_kernel_refinement
#print axioms executable_has_decision_contract

end ReleaseEvidenceOracle

set_option pp.fullNames true in
#check ReleaseEvidenceOracle.decision_kernel_refinement
set_option pp.fullNames true in
#check ReleaseEvidenceOracle.checkedDecision
set_option pp.fullNames true in
#check ReleaseEvidenceOracle.executable_has_decision_contract

open ReleaseEvidenceOracle Lean in
def main (args : List String) : IO UInt32 := do
  match args with
  | ["--corpus", path] =>
    let corpus ← decodeFile GeneratedInputs path
    if corpus.schema != "reb-lean-assessment-inputs/1" || corpus.seed != "0x5245424c45414e37" || corpus.cases.size != 1024 then
      throw (IO.userError "Expected the preregistered schema, seed and 1,024 assessment inputs")
    for sample in corpus.cases do
      let result := checkedDecision.val sample.input
      IO.println ((Json.mkObj [("id", toJson sample.id), ("decision", toJson result)]).compress)
  | [] =>
    throw (IO.userError "Usage: lean --run IndependentOracle.lean INPUT.json [INPUT.json ...] or --corpus INPUTS.json")
  | paths =>
    for path in paths do
      let value ← decodeFile Input path
      IO.println ((toJson (checkedDecision.val value)).compress)
  return 0
