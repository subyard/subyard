package reconcileruntime

import (
	"os"
	"strconv"
	"strings"
)

func (runtime Runtime) bindingPath(value string) string {
	if runtime.LogicalRepositoryRoot != "" && runtime.RepositoryRoot != "" && runtime.LogicalRepositoryRoot != runtime.RepositoryRoot {
		// Profile executable paths name this process explicitly so children can
		// read our retained descriptor. It is the same checked repository OFD,
		// and its process-local spelling is not a desired source identity.
		if strings.HasPrefix(runtime.RepositoryRoot, "/proc/self/fd/") {
			alias := strings.Replace(runtime.RepositoryRoot, "/proc/self/", "/proc/"+strconv.Itoa(os.Getpid())+"/", 1)
			value = replaceRepositoryLabel(value, alias, runtime.LogicalRepositoryRoot)
		}
		return replaceRepositoryLabel(value, runtime.RepositoryRoot, runtime.LogicalRepositoryRoot)
	}
	return value
}

func replaceRepositoryLabel(value, physical, logical string) string {
	var result strings.Builder
	for {
		index := strings.Index(value, physical)
		if index < 0 {
			result.WriteString(value)
			return result.String()
		}
		end := index + len(physical)
		result.WriteString(value[:index])
		start := index == 0 || strings.ContainsRune("=:\"'\n\r\t [,(", rune(value[index-1]))
		if start && (end == len(value) || strings.ContainsRune("/\"'\n\r\t ", rune(value[end]))) {
			result.WriteString(logical)
		} else {
			result.WriteString(physical)
		}
		value = value[end:]
	}
}

func (runtime Runtime) bindingEnvironment(environment []string) []string {
	values := configBindingEnvironment(environment)
	for index := range values {
		values[index] = runtime.bindingPath(values[index])
	}
	return values
}
