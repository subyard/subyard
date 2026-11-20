package ports

// RollbackSupport contains component observations, not release eligibility.
// Binding seals the private read set without exporting paths or settings.
type RollbackSupport struct {
	Consumers []RollbackConsumer `json:"consumers"`
	Binding   string             `json:"binding"`
}

type RollbackConsumer struct {
	Kind     string `json:"kind"`
	Yard     string `json:"yard"`
	Owner    string `json:"owner"`
	Profile  string `json:"profile,omitempty"`
	Selected bool   `json:"selected"`
	// Disableable reports a profile whose `profile disable` deactivates its runtime owner.
	Disableable         bool                    `json:"disableable,omitempty"`
	ManagedPaths        int                     `json:"managedPaths"`
	ManagedPathsPresent bool                    `json:"managedPathsPresent"`
	RetainedContract    string                  `json:"retainedContract"`
	Observation         *RuntimeObservation     `json:"observation"`
	RetainedPayload     string                  `json:"retainedPayload"`
	RetainedSettings    string                  `json:"retainedSettings"`
	Installed           []RollbackInstalledPart `json:"installed"`
}

type RollbackInstalledPart struct {
	Kind     string `json:"kind"`
	Present  bool   `json:"present"`
	Payload  string `json:"payload"`
	Settings string `json:"settings"`
}
