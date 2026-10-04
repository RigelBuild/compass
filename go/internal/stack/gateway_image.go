//go:build unix

package stack

// DefaultGatewayImage is the gateway image the installed stack runs, pinned by digest.
// Empty means no default: up needs --gateway-image or --gateway-external.
const DefaultGatewayImage = ""
