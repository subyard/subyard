package releaseruntime

import "fmt"

// compiledReleaseOwner is set only by the build linker. Runtime inputs cannot
// change release authority; unknown build values refuse before inspection.
var compiledReleaseOwner = "go"

func CompiledRustReleaseOwner() (bool, error) {
	switch compiledReleaseOwner {
	case "go":
		return false, nil
	case "rust":
		return true, nil
	default:
		return false, markSelectedRustOwnerError(fmt.Errorf("invalid compiled release owner %q", compiledReleaseOwner))
	}
}
