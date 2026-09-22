package transport

import "context"

type sshTrustKey struct{}

// WithSSHTrust installs an invocation-scoped trust gate. Explicitly pinned and
// key-assessment transports do not use it. The original payload is never retried.
func WithSSHTrust(ctx context.Context, gate func(context.Context, string, string) ([]string, error)) context.Context {
	return context.WithValue(ctx, sshTrustKey{}, gate)
}

func SSHOptions(ctx context.Context, program, target string) ([]string, error) {
	gate, _ := ctx.Value(sshTrustKey{}).(func(context.Context, string, string) ([]string, error))
	if gate == nil {
		return nil, nil
	}
	return gate(ctx, program, target)
}
