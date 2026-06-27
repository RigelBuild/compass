//! `compassd` — the Compass daemon entry point.
//!
//! The long-lived backend that owns all privileged operations (agent
//! processes, PTYs, the Warden security layer, the taint registry, VCS/issue
//! integration) and serves the `compass.v1` contract over a platform-local
//! transport. This scaffold is the entry point only; lifecycle (start/stop/
//! status) and the transport server land in SEA-1025.

use anyhow::Result;
use clap::Parser;

/// The Compass daemon.
#[derive(Parser, Debug)]
#[command(name = "compassd", version, about)]
struct Cli {}

fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into()),
        )
        .init();

    let _cli = Cli::parse();

    let version = env!("CARGO_PKG_VERSION");
    tracing::info!(
        version,
        api = compass_proto::API_VERSION,
        "compassd starting"
    );
    println!("compassd {version} ({})", compass_proto::API_VERSION);
    Ok(())
}
