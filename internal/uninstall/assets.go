package uninstall

// CaptureAssets uses teardown's metadata guard and the installed manifest or
// existing public source selector. Operator overlays never enter recovery.
func CaptureAssets(root string) ([]Tree, error) {
	var assets []Tree
	err := nativeArtifact("capture-assets", root, &assets)
	return assets, err
}
