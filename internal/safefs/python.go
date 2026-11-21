package safefs

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
	"strconv"
	"strings"
)

//go:embed safefs.py
var pythonSource string

const pythonMarker = "import safefs  # safefs: inline"

// PythonBootstrap loads the embedded module in one physical line.
func PythonBootstrap() string {
	return "import sys as _s, types as _t; safefs = _s.modules.get('safefs') or _t.ModuleType('safefs'); " +
		"'safefs' in _s.modules or (exec(compile(" + strconv.Quote(pythonSource) +
		", 'safefs.py', 'exec'), safefs.__dict__), _s.modules.__setitem__('safefs', safefs))" +
		fmt.Sprintf("  # safefs-inline sha256=%x", sha256.Sum256([]byte(pythonSource)))
}

// InlinePython replaces a single marker line without shifting traceback locations.
func InlinePython(program string) (string, error) {
	lines := strings.Split(program, "\n")
	marker := -1
	for i, line := range lines {
		if line == pythonMarker {
			if marker >= 0 {
				return "", fmt.Errorf("safefs: multiple Python import markers")
			}
			marker = i
		}
	}
	if marker < 0 {
		return "", fmt.Errorf("safefs: missing Python import marker")
	}
	lines[marker] = PythonBootstrap()
	return strings.Join(lines, "\n"), nil
}
