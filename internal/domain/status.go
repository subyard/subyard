package domain

type StartState string

const StartWaitingForAddress StartState = "waiting-for-address"

type SharedResourceStatus struct {
	Profile string `json:"profile"`
	Name    string `json:"name"`
	State   string `json:"state"`
	Hint    string `json:"hint,omitempty"`
	URL     string `json:"url,omitempty"`
}

type AgentStatus struct {
	Name               string `json:"name"`
	State              string `json:"state"`
	Hint               string `json:"hint,omitempty"`
	URL                string `json:"url,omitempty"`
	DashboardPort      int    `json:"dashboardPort,omitempty"`
	DashboardGuestPort int    `json:"-"`
	DashboardScheme    string `json:"-"`
	DashboardPath      string `json:"-"`
}

type StatusFacts struct {
	Profiles []string               `json:"profiles"`
	Agents   []AgentStatus          `json:"agents"`
	Shared   []SharedResourceStatus `json:"shared"`
	Security string                 `json:"security"`
	Space    string                 `json:"space"`
}

// ReleaseReadiness describes the owner host's installed release, shared by its yards.
type ReleaseReadiness struct {
	Status  string `json:"status"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Next    string `json:"next,omitempty"`
}

type YardStatus struct {
	Context              Context           `json:"context"`
	ResolvedYardImage    ResolvedYardImage `json:"resolvedYardImage,omitempty"`
	State                string            `json:"state"`
	StartState           StartState        `json:"startState,omitempty"`
	DeferredPublications []string          `json:"deferredPublications,omitempty"`
	WaitingForAddresses  []string          `json:"waitingForAddresses,omitempty"`
	Desired              string            `json:"desired"`
	Initialized          string            `json:"initialized"`
	IncusAutostart       string            `json:"incusAutostart"`
	IP                   string            `json:"ip,omitempty"`
	SSHConfigured        bool              `json:"sshConfigured"`
	Mounts               []string          `json:"mounts"`
	Services             string            `json:"services,omitempty"`
	VSCode               string            `json:"vscode,omitempty"`
	ProjectCount         int               `json:"projectCount"`
	Facts                StatusFacts       `json:"facts"`
}
