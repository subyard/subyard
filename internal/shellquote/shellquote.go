package shellquote

import "strings"

func Word(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func Command(arguments []string) string {
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		quoted[index] = Word(argument)
	}
	return strings.Join(quoted, " ")
}

// RecoveryCommand respects the frozen release protocol, which rejects shell
// separators even in quoted arguments. Bash decodes the literal bytes inside
// the one exec command, so the operator can paste it into a POSIX shell.
func RecoveryCommand(arguments []string) string {
	command := Command(arguments)
	if !strings.ContainsAny(command, ";&|") {
		return command
	}
	escape := strings.NewReplacer(`\`, `\\`, "'", `\'`, ";", `\073`, "&", `\046`, "|", `\174`)
	words := make([]string, len(arguments))
	for index, argument := range arguments {
		words[index] = "$'" + escape.Replace(argument) + "'"
	}
	return Command([]string{"bash", "-c", "exec " + strings.Join(words, " ")})
}
