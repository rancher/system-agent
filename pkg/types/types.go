package types

type Instruction struct {
	Name    string   `json:"name,omitempty"`
	Image   string   `json:"image,omitempty"`
	Env     []string `json:"env,omitempty"`
	Args    []string `json:"args,omitempty"`
	Command string   `json:"command,omitempty"`
}

// Name would be `ca.pem`, Path would be `/etc/kubernetes/ssl`, Contents is base64 encoded
type File struct {
	Content string `json:"content,omitempty"`
	Name    string `json:"name,omitempty"`
	Path    string `json:"path,omitempty"`
}

type NodePlan struct {
	Files        []File        `json:"files,omitempty"`
	Instructions []Instruction `json:"instructions,omitempty"`
}

type NodePlanPosition struct {
	AppliedChecksum string `json:"appliedChecksum,omitempty"`
}

type AgentNodePlan struct {
	Plan     NodePlan
	Checksum string
}
