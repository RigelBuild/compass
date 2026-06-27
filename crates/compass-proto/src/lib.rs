//! `compass.v1` contract: the generated gRPC client + server stubs (checked in
//! and CI drift-gated against `proto/compass/v1/`). A generated client is the
//! only sanctioned way to reach the daemon; raw stub/socket access is fenced
//! off elsewhere.
//!
//! Permissively licensed (carve-out from the workspace AGPL default) so
//! third-party UIs, the native client, and enterprise builds can link the wire
//! protocol without taking AGPL.

/// Generated `compass.v1` messages + gRPC client/server stubs. The prost
/// output `include!`s the tonic client/server in the same module.
pub mod v1 {
    #![allow(clippy::all, clippy::pedantic, clippy::nursery)]
    include!("gen/compass/v1/compass.v1.rs");
}

/// The contract version this crate defines.
pub const API_VERSION: &str = "compass.v1";
