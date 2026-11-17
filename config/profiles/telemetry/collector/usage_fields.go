package collector

import (
	"encoding/json"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/Subyard/Subyard/config/profiles/telemetry"
)

type usageField struct {
	Type         string
	Values       []string
	Aliases      []string
	Required     bool
	ToolRequired bool `json:"tool_required"`
}

var usageSchema = func() (schema struct {
	Fields      map[string]usageField
	Attributes  map[string]usageField
	NamePattern string `json:"name_pattern"`
	IntPattern  string `json:"int_pattern"`
	TimePattern string `json:"time_pattern"`
}) {
	if err := json.Unmarshal([]byte(telemetry.UsageFields), &schema); err != nil {
		panic(err)
	}
	for key, field := range schema.Fields {
		schema.Attributes[key] = field
		for _, alias := range field.Aliases {
			schema.Attributes[alias] = field
		}
	}
	return schema
}()

var (
	usageFieldNames = slices.Sorted(maps.Keys(usageSchema.Fields))
	usageName       = regexp.MustCompile("^(" + usageSchema.NamePattern + ")$")
	usageInt        = regexp.MustCompile("^(" + usageSchema.IntPattern + ")$")
	usageTime       = regexp.MustCompile("^(" + usageSchema.TimePattern + ")$")
)

func usageTimestamp(value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return parsed, err == nil && usageTime.MatchString(value) && parsed.Year() > 0
}

func (field usageField) validate(value any) (any, bool) {
	text, isString := value.(string)
	switch field.Type {
	case "name":
		return text, isString && usageName.MatchString(text)
	case "enum":
		return text, isString && slices.Contains(field.Values, text)
	case "time":
		_, ok := usageTimestamp(text)
		return text, isString && ok
	case "int":
		switch v := value.(type) {
		case int:
			return int64(v), v >= 0
		case int64:
			return v, v >= 0
		case json.Number:
			text = string(v)
		case string:
		default:
			return nil, false
		}
		n, err := strconv.ParseInt(text, 10, 64)
		return n, err == nil && usageInt.MatchString(text)
	default:
		return nil, false
	}
}
