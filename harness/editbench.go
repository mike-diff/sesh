package harness

// Seams for the editbench rig (bench/editbench_test.go), which drives the real
// library loop with the real built-in toolset. builtinTools is unexported (it
// is product policy, not library API), so this wrapper is the rig's way in.

import "github.com/mike-diff/sesh/agent"

// BenchTools returns the built-in tools for the editbench rig. unsafePaths
// mirrors the -unsafe-paths flag (the rig passes false: its fixtures live in
// the working directory).
func BenchTools(unsafePaths bool) []agent.Tool {
	return builtinTools(unsafePaths, nil)
}

// BenchCredential resolves a provider's stored API key the way the product
// does, encrypted store included, so the rig can drive providers whose keys
// never touch the environment (zai's lives only in credentials.json). Resolve
// it BEFORE sandboxing HOME.
func BenchCredential(provider string) string {
	return loadCredentials()[provider]
}

// BenchSetDiffLines points the diff_lines dial for an A/B rig run (the rig
// runs in the bench package, where the live dial set is unreachable).
func BenchSetDiffLines(n int) {
	tune.DiffLines = n
}
