package main

import "flag"

func upstreamLimitFlags() *upstreamLimits {
	limits := new(upstreamLimits)
	flag.IntVar(&limits.registries, "upstream-registries", 128, "maximum tracked upstream registries")
	flag.IntVar(&limits.scopes, "upstream-scopes-per-registry", 64, "maximum reusable pull scopes per upstream registry")
	flag.Float64Var(&limits.requestsPerSecond, "upstream-requests-per-second", 16, "outbound HTTP requests per second per upstream registry")
	flag.IntVar(&limits.burst, "upstream-request-burst", 16, "outbound HTTP burst per upstream registry")
	return limits
}
