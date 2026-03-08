package agent

import (
	"time"

	"github.com/gonkalabs/gonka-agent/internal/roles"
)

// Phase is a step in the 6-phase execution state machine.
type Phase string

const (
	PhaseUnderstand Phase = "UNDERSTAND"
	PhasePlan       Phase = "PLAN"
	PhasePreVerify  Phase = "PRE_VERIFY"
	PhaseExecute    Phase = "EXECUTE"
	PhaseValidate   Phase = "VALIDATE"
	PhaseConfirm    Phase = "CONFIRM"
)

// ErrorKind classifies a failure for targeted recovery.
type ErrorKind int

const (
	ErrTransient  ErrorKind = iota // network/502 → retry x3
	ErrBuildFail                   // compiler error → auto-debug loop
	ErrTestFail                    // test failure → debug loop
	ErrScopeBad                    // wrong occurrences → git_reset_file + re-plan
	ErrDeadEnd                     // same error 2+ times → rollback all + re-plan
	ErrDrift                       // model ignoring original task → reinforce
	ErrFatal                       // workspace escape / catastrophic → abort
)

// BuildError is a parsed compiler error.
type BuildError struct {
	File    string
	Line    int
	Col     int
	Message string
}

// AgentState carries all mutable state across the 6 phases.
type AgentState struct {
	Phase        Phase
	OriginalTask string
	Plan         *roles.FinalPlan

	// Execution tracking
	StepCount    int
	RePlanCount  int // max 3 cycles before abort
	ErrorCounts  map[string]int // error_signature → count for dead-end detection

	// Rollback registry: path → original content (set before first edit)
	OriginalContent map[string]string
	ModifiedFiles   []string

	// Evidence collected during VALIDATE
	EvidenceTable string

	// Final Scientist verdict
	FinalVerdict roles.ScientistFrame

	// Timing
	StartTime time.Time

	// LLM client reference for token reporting
	LLM interface{ TokenSummary() string }

	// Progress callback — receives real-time events during execution.
	Progress ProgressFn
}

func newAgentState(task string) *AgentState {
	return &AgentState{
		Phase:           PhaseUnderstand,
		OriginalTask:    task,
		ErrorCounts:     map[string]int{},
		OriginalContent: map[string]string{},
		StartTime:       time.Now(),
	}
}
