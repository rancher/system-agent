package types

import (
	"crypto/sha256"
	"fmt"
)

type Instruction struct {
	Image string `json:"image,omitempty"`
	Env []string `json:"env,omitempty"`
	Args []string `json:"args,omitempty"`
	Command string `json:"command,omitempty"`
}

type NodePlan struct {
	Instructions []Instruction `json:"instructions,omitempty"`
	AgentCheckInterval int `json:"agentCheckInterval,omitempty"`
}

func (n NodePlan) Checksum() string {
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("%v", n)))

	return fmt.Sprintf("%x", h.Sum(nil))
}


type NodePlanPosition struct {
	AppliedChecksum string `json:"appliedChecksum,omitempty"`
}