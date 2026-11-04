package cli

// The first separator starts opaque handler arguments, including consent-like
// flags that cannot authorize the enclosing command.
func commandAssumeYes(arguments []string) bool {
	for _, argument := range arguments {
		if argument == "--" {
			break
		}
		if argument == "--yes" || argument == "-y" {
			return true
		}
	}
	return false
}

func withoutCommandConsent(arguments []string) []string {
	result := make([]string, 0, len(arguments))
	for index, argument := range arguments {
		if argument == "--" {
			return append(result, arguments[index:]...)
		}
		if argument != "-y" && argument != "--yes" {
			result = append(result, argument)
		}
	}
	return result
}
