# GoReleaser builds this image from the binaries it has already compiled: the
# build context holds them as <os>/<arch>/statica, and there is nothing to
# compile or run here. Dependabot updates the pinned base image digest.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/statica /usr/bin/statica
ENV STATICA_CONFIG=/etc/statica/config.yaml
EXPOSE 8080 9090
USER 65532:65532
ENTRYPOINT ["/usr/bin/statica"]
CMD ["serve"]
