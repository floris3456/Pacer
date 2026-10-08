package pluginabi

// Additive schema-7 methods; the C ABI and existing JSON envelope are unchanged.
const (
	SchemaVersionCachePolicy uint32 = 7
	MethodCachePolicyHandle         = "cache_policy.handle"
	MethodCachePolicyResolve        = "cache_policy.resolve"
	MethodCachePolicyAttempt        = "cache_policy.attempt"
)
