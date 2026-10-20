// Package agentidentity defines the shared identity used for agent commits.
package agentidentity

// AgentCommitEmail returns the reserved email for an agent account's commits.
func AgentCommitEmail(accountID string) string {
	return accountID + "@agents.compass.invalid"
}
