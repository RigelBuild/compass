//go:build unix

// The Runner-side FetchSecrets client: pull a session's resolved secret set from
// the Server over the RunnerService connection and map the wire ResolvedSecret
// back to the secrets-package resolve-surface type at this edge — the reverse of
// runnerhub's resolvedSecretToProto, keeping the two enum translations symmetric.
// The resolved values ride in memory only; nothing here logs a value.
package runner

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/secrets"
)

// FetchSecrets fetches the resolved secret set for a live session from the
// Server (the rotation re-fetch — the SecretsVersion signal path), returning it
// as the secrets-package resolve-surface type the materializer routes on. A
// transport/authz failure surfaces as an error (never a silent empty set), so
// the caller can log it and recover on the next signal.
func (l *ServerLink) FetchSecrets(ctx context.Context, sessionID string) ([]secrets.ResolvedSecret, error) {
	return l.fetchSecrets(ctx, &compassv1internal.FetchSecretsRequest{
		SessionId: sessionID,
	}, fmt.Sprintf("session %q", sessionID))
}

// FetchSecretsByContainer fetches the resolved secret set for a provisioned
// container from the Server — the PROVISION-time initial materialize, before a
// session exists. Authorized on the container→account binding recorded at
// Provision. Same inject-all set as FetchSecrets; only the authz selector
// differs.
func (l *ServerLink) FetchSecretsByContainer(ctx context.Context, containerName string) ([]secrets.ResolvedSecret, error) {
	return l.fetchSecrets(ctx, &compassv1internal.FetchSecretsRequest{
		ContainerName: containerName,
	}, fmt.Sprintf("container %q", containerName))
}

// fetchSecrets issues the FetchSecrets RPC and maps the wire set to the
// resolve-surface type. subject names the binding for the error message.
func (l *ServerLink) fetchSecrets(ctx context.Context, req *compassv1internal.FetchSecretsRequest, subject string) ([]secrets.ResolvedSecret, error) {
	resp, err := l.client.FetchSecrets(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, fmt.Errorf("fetching secrets for %s: %w", subject, err)
	}
	wire := resp.Msg.GetSecrets()
	out := make([]secrets.ResolvedSecret, 0, len(wire))
	for _, s := range wire {
		out = append(out, resolvedSecretFromProto(s))
	}
	return out, nil
}

// resolvedSecretFromProto maps a wire ResolvedSecret to the resolve-surface type,
// translating the public proto delivery/kind enums to the secrets-package enums —
// the reverse of runnerhub.resolvedSecretToProto. Value/version/host/provider
// ride through verbatim.
func resolvedSecretFromProto(s *compassv1internal.ResolvedSecret) secrets.ResolvedSecret {
	return secrets.ResolvedSecret{
		Name:     s.GetName(),
		Value:    s.GetValue(),
		Version:  s.GetVersion(),
		Delivery: deliveryFromProto(s.GetDelivery()),
		Kind:     kindFromProto(s.GetKind()),
		Host:     s.GetHost(),
		Provider: s.GetProvider(),
	}
}

// deliveryFromProto maps the public proto delivery enum to the resolve-surface
// enum. The proto reserves 0 for UNSPECIFIED; anything but ENV is File (the
// rotatable default).
func deliveryFromProto(d compassv1.SecretDelivery) secrets.DeliveryKind {
	if d == compassv1.SecretDelivery_SECRET_DELIVERY_ENV {
		return secrets.DeliveryEnv
	}
	return secrets.DeliveryFile
}

// kindFromProto maps the public proto kind enum to the resolve-surface enum. The
// proto reserves 0 for UNSPECIFIED; an unknown kind falls back to Generic.
func kindFromProto(k compassv1.SecretKind) secrets.SecretKind {
	switch k {
	case compassv1.SecretKind_SECRET_KIND_PROVIDER:
		return secrets.SecretProvider
	case compassv1.SecretKind_SECRET_KIND_GH:
		return secrets.SecretGH
	default:
		return secrets.SecretGeneric
	}
}
