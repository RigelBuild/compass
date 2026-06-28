//! Smoke tests: the generated compass.v1 contract is present and usable, and
//! the client + server stubs the daemon and clients build against exist.

use compass_proto::v1::GetDaemonInfoResponse;
use compass_proto::v1::compass_service_client::CompassServiceClient;
use compass_proto::v1::compass_service_server::CompassService;

#[test]
fn api_version_is_v1() {
    assert_eq!(compass_proto::API_VERSION, "compass.v1");
}

#[test]
fn generated_message_round_trips_fields() {
    let resp = GetDaemonInfoResponse {
        version: "0.1.0".into(),
        api_version: compass_proto::API_VERSION.into(),
    };
    assert_eq!(resp.version, "0.1.0");
    assert_eq!(resp.api_version, "compass.v1");
}

// Compile-time proof the generated gRPC client type exists — what every client
// (TS, native) is built against.
#[allow(dead_code)]
type GeneratedClient = CompassServiceClient<tonic::transport::Channel>;

// Compile-time proof the generated server trait exists — what the daemon impls.
#[allow(dead_code)]
fn uses_server_trait<T: CompassService>() {}
