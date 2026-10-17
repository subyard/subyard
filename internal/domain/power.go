package domain

// DesiredPower is the durable power intent, independent of observed instance status.
type DesiredPower string

const (
	PowerRunning DesiredPower = "running"
	PowerStopped DesiredPower = "stopped"
)
