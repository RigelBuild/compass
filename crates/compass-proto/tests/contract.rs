//! Smoke tests: the generated compass.v1 contract is present and usable, and
//! the client + server stubs the daemon and clients build against exist.

use compass_proto::v1::GetDaemonInfoResponse;
use compass_proto::v1::compass_service_client::CompassServiceClient;
use compass_proto::v1::compass_service_server::CompassService;
use compass_proto::v1::{
    DaemonState, DaemonStatus, SubscribeEventsRequest, SubscribeEventsResponse,
    subscribe_events_response,
};

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

#[test]
fn event_carries_seq_and_typed_payload() {
    let ev = SubscribeEventsResponse {
        seq: 42,
        at_unix_ms: 1_700_000_000_000,
        payload: Some(subscribe_events_response::Payload::DaemonStatus(
            DaemonStatus {
                state: DaemonState::Ready as i32,
            },
        )),
    };
    assert_eq!(ev.seq, 42);
    assert_eq!(ev.at_unix_ms, 1_700_000_000_000);
    match ev.payload {
        Some(subscribe_events_response::Payload::DaemonStatus(s)) => {
            assert_eq!(s.state(), DaemonState::Ready)
        }
        _ => panic!("expected daemon_status payload"),
    }
}

#[test]
fn daemon_state_enum_is_stable() {
    assert_eq!(DaemonState::Unspecified as i32, 0);
    assert_eq!(DaemonState::Ready as i32, 1);
    assert_eq!(DaemonState::Ready.as_str_name(), "DAEMON_STATE_READY");
}

#[test]
fn subscribe_events_request_defaults_to_snapshot() {
    // since_seq = 0 is the snapshot-then-tail cursor.
    assert_eq!(SubscribeEventsRequest::default().since_seq, 0);
}
