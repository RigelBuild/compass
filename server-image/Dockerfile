# syntax=docker/dockerfile:1.7-labs
# The compass-server OCI image carries only the realised server closure.
FROM gcr.io/distroless/static-debian12@sha256:c0f429e16b13e583da7e5a6ec20dd656d325d88e6819cafe0adb0828976529dc

ARG SOURCE_DATE_EPOCH=1
COPY store /nix/store

USER 65532:65532
ENTRYPOINT ["/nix/store/.compass-server"]
