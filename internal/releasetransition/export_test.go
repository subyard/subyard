package releasetransition

// SetProcessCrashFaultForTest attaches only existing fault seams. This file is
// absent from production builds; it creates no execution authority.
func SetProcessCrashFaultForTest(transition *V2Transition, links *RuntimeLinkStore, fault func(string, string) error) {
	transition.options.fault = func(point string) error { return fault("transition", point) }
	transition.store.fault = func(point string) error { return fault("store", point) }
	links.fault = func(point string) error { return fault("links", point) }
}
