//go:build unix

package stack

// DefaultGatewayImage is pinned by digest and empty until publication; installed up must pass --gateway-image or --gateway-external.
const DefaultGatewayImage = ""
