package cli

import (
	"context"

	"github.com/Subyard/Subyard/internal/yardnetwork"
)

type testNetworkPolicyHost struct{ yardnetwork.Host }

func (testNetworkPolicyHost) ReadPolicy(context.Context) (yardnetwork.StoredPolicy, error) {
	policy, err := yardnetwork.Decode(nil)
	return yardnetwork.StoredPolicy{Policy: policy}, err
}

type testNetworkPolicyLock struct{}

func (testNetworkPolicyLock) Acquire(context.Context) (func(), error) {
	return func() {}, nil
}

func allowTestNetworkPolicy() *yardnetwork.Service {
	return &yardnetwork.Service{Host: testNetworkPolicyHost{}, Lock: testNetworkPolicyLock{}}
}
