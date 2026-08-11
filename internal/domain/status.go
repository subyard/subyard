package domain

type SharedResourceStatus struct {
	Profile string `json:"profile"`
	Name    string `json:"name"`
	State   string `json:"state"`
	Hint    string `json:"hint,omitempty"`
}

type StatusFacts struct {
	Shared   []SharedResourceStatus `json:"shared"`
	Security string                 `json:"security"`
	Space    string                 `json:"space"`
}

type YardStatus struct {
	Context           Context           `json:"context"`
	ResolvedYardImage ResolvedYardImage `json:"resolvedYardImage,omitempty"`
	State             string            `json:"state"`
	Desired           string            `json:"desired"`
	Initialized       string            `json:"initialized"`
	IncusAutostart    string            `json:"incusAutostart"`
	IP                string            `json:"ip,omitempty"`
	SSHConfigured     bool              `json:"sshConfigured"`
	Mounts            []string          `json:"mounts"`
	Services          string            `json:"services,omitempty"`
	VSCode            string            `json:"vscode,omitempty"`
	ProjectCount      int               `json:"projectCount"`
	Facts             StatusFacts       `json:"facts"`
}
