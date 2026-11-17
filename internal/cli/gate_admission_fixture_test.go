package cli

// admitPreparedFixture explicitly exempts fixtures that prepare commands outside
// Run or RPC. Preserve admissions supplied by gate-specific tests.
func admitPreparedFixture(cli *CLI) *CLI {
	if cli.gateAdmitted == nil {
		cli.gateAdmitted = &gateAdmission{kind: admissionExempt}
	}
	return cli
}
