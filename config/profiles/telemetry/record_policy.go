// Package telemetry shares the record policy with the Python producers.
package telemetry

import _ "embed"

//go:embed record-policy.txt
var RecordPolicy string
