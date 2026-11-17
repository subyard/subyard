// Package telemetry embeds the field table shared with the Python producer.
package telemetry

import _ "embed"

//go:embed usage-fields.json
var UsageFields string
