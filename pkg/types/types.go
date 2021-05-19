package types

type Instruction struct {
	Name       string   `json:"name,omitempty"`
	SaveOutput bool     `json:"saveOutput,omitempty"`
	Image      string   `json:"image,omitempty"`
	Env        []string `json:"env,omitempty"`
	Args       []string `json:"args,omitempty"`
	Command    string   `json:"command,omitempty"`
}

// Path would be `/etc/kubernetes/ssl/ca.pem`, Content is base64 encoded.
// If Directory is true, then we are creating a directory, not a file
type File struct {
	Content     string `json:"content,omitempty"`
	Directory   bool   `json:"directory,omitempty"`
	UID         int    `json:"uid,omitempty"`
	GID         int    `json:"gid,omitempty"`
	Path        string `json:"path,omitempty"`
	Permissions string `json:"permissions,omitempty"` // internally, the string will be converted to a uint32 to satisfy os.FileMode
}

type NodePlan struct {
	Files        []File           `json:"files,omitempty"`
	Instructions []Instruction    `json:"instructions,omitempty"`
	Probes       map[string]Probe `json:"probes,omitempty"`
}

// stdout and stderr are both base64, gzipped
type NodePlanPosition struct {
	AppliedChecksum string                 `json:"appliedChecksum,omitempty"`
	Output          []byte                 `json:"output,omitempty"`
	ProbeStatus     map[string]ProbeStatus `json:"probeStatus,omitempty"`
}

// AgentNodePlan is passed into Applyinator
type AgentNodePlan struct {
	Plan     NodePlan
	Checksum string
}

type HttpGetAction struct {
	Path       string `json:"path,omitempty"`
	ClientCert string `json:"clientCert,omitempty"`
	ClientKey  string `json:"clientKey,omitempty"`
	CACert     string `json:"caCert,omitempty"`
}

type Probe struct {
	Name                string        `json:"name,omitempty"`
	InitialDelaySeconds int           `json:"initialDelaySeconds,omitempty"` // default 0
	TimeoutSeconds      int           `json:"timeoutSeconds,omitempty"`      // default 1
	SuccessThreshold    int           `json:"successThreshold,omitempty"`    // default 1
	FailureThreshold    int           `json:"failureThreshold,omitempty"`    // default 3
	HttpGetAction       HttpGetAction `json:"httpGet,omitempty"`
}

type ProbeStatus struct {
	Healthy      bool `json:"healthy,omitempty"`
	SuccessCount int  `json:"successCount,omitempty"`
	FailureCount int  `json:"failureCount,omitempty"`
}
