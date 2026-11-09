# GoReleaser builds this image from the binaries it has already compiled: the
# build context holds them as <os>/<arch>/statica, and there is nothing to
# compile or run here. Dependabot updates the pinned base image digest.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/statica /usr/bin/statica
ENV STATICA_CONFIG=/etc/statica/config.yaml
EXPOSE 8080 9090
USER 65532:65532
ENTRYPOINT ["/usr/bin/statica"]
CMD ["serve"]
