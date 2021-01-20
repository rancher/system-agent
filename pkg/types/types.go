package types

import (
	"crypto/sha256"
	"fmt"
)

type Instruction struct {
	Name string `json:"name,omitempty"`
	Image string `json:"image,omitempty"`
	Env []string `json:"env,omitempty"`
	Args []string `json:"args,omitempty"`
	Command string `json:"command,omitempty"`
}

// Name would be `ca.pem`, Path would be `/etc/kubernetes/ssl`, Contents is base64 encoded
type File struct {
	Content string `json:"content,omitempty"`
	Name     string `json:"name,omitempty"`
	Path     string `json:"path,omitempty"`
}

type NodePlan struct {
	Files []File `json:"files,omitempty"`
	Instructions []Instruction `json:"instructions,omitempty"`
}

func (n NodePlan) Checksum() string {
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("%v", n)))

	return fmt.Sprintf("%x", h.Sum(nil))
}


type NodePlanPosition struct {
	AppliedChecksum string `json:"appliedChecksum,omitempty"`
}