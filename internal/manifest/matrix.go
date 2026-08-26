package manifest

import "fmt"

var supportedTargets = []struct {
	target Target
	runner string
}{
	{Target{OS: "darwin", Arch: "amd64"}, "macos-15-intel"},
	{Target{OS: "darwin", Arch: "arm64"}, "macos-15"},
	{Target{OS: "linux", Arch: "386"}, "ubuntu-24.04"},
	{Target{OS: "linux", Arch: "amd64"}, "ubuntu-24.04"},
	{Target{OS: "linux", Arch: "arm64"}, "ubuntu-24.04-arm"},
	{Target{OS: "windows", Arch: "386"}, "windows-2022"},
	{Target{OS: "windows", Arch: "amd64"}, "windows-2022"},
	{Target{OS: "windows", Arch: "arm64"}, "windows-11-arm"},
}

// RunnerForTarget returns the allowlisted GitHub-hosted runner for a target.
// Manifest data cannot inject arbitrary runner labels.
func RunnerForTarget(target Target) (string, error) {
	for _, supported := range supportedTargets {
		if target == supported.target {
			return supported.runner, nil
		}
	}
	return "", fmt.Errorf("unsupported target %q", target.Key())
}

// Matrix returns one row per unique target in deterministic allowlist order.
// Its JSON representation is suitable for a workflow's fromJSON() matrix:
// {"include":[{"target":"windows/amd64","runner":"windows-2022","key":"windows-amd64"}]}.
func (m *Manifest) Matrix() Matrix {
	present := make(map[Target]struct{}, len(m.Plans))
	for _, plan := range m.Plans {
		present[plan.Target] = struct{}{}
	}
	matrix := Matrix{Include: make([]MatrixEntry, 0, len(present))}
	for _, supported := range supportedTargets {
		if _, ok := present[supported.target]; !ok {
			continue
		}
		matrix.Include = append(matrix.Include, MatrixEntry{
			Target: supported.target.Key(),
			Runner: supported.runner,
			Key:    supported.target.OS + "-" + supported.target.Arch,
		})
	}
	return matrix
}

// PlansForTarget returns a copy of the plans for an os/arch key.
func (m *Manifest) PlansForTarget(target string) []Plan {
	plans := make([]Plan, 0)
	for _, plan := range m.Plans {
		if plan.Target.Key() == target {
			plans = append(plans, plan)
		}
	}
	return plans
}
