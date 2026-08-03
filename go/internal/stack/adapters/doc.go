//go:build unix

// Package adapters holds the real seam adapters for the T2 stack supervisor's
// green-backed effects: the TLS anchor ensured via internal/certgen and the
// GetServerInfo readiness probe issued via the generated compass.v1 client over
// the server's unix socket. The red-backed adapters (runner-token mint,
// socket-path default) and the process/image adapters live in sibling slices;
// this package deliberately imports only green packages so it builds and tests
// in isolation on the base-red main.
package adapters
