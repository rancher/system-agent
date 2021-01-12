package types

type Instruction struct {
	Image string `json:"image,omitempty"`
	Env []string `json:"env,omitempty"`
	Args []string `json:"args,omitempty"`
	Command string `json:"command,omitempty"`
}

type NodePlan struct {
	Instructions []Instruction `json:"instructions,omitempty"`
	Version int `json:"version,omitempty"`
	AgentCheckInterval int `json:"agentCheckInterval,omitempty"`
}

type NodePlanPosition struct {
	AppliedVersion int `json:"appliedVersion,omitempty"`
	PlanChecksum string `json:"planChecksum,omitempty"`
}