# Build from committed vendor sources or the versions in go.mod and go.sum.
ARG UBI_IMAGE=registry.access.redhat.com/ubi9/ubi-minimal@sha256:34880b64c07f28f64d95737f82f891516de9a3b43583f39970f7bf8e4cfa48b7
ARG GO_IMAGE=golang@sha256:fe517acaa580cc7915457080f850c37e618b3c1e750f98b58fc6429b605147a1

FROM ${GO_IMAGE} AS build
ENV CGO_ENABLED=0 GOWORK=off
WORKDIR /src
COPY . .

RUN set -e; \
    if [ -f vendor/modules.txt ]; then \
      GOPROXY=off go build -mod=vendor -trimpath -o /out/manager ./cmd/manager; \
    else \
      go build -mod=readonly -trimpath -o /out/manager ./cmd/manager; \
    fi

FROM ${UBI_IMAGE}

ARG VERSION=26.01
ARG RELEASE=dev
ARG VCS_REF=unknown
# Driver-contract provenance inputs (build/image.py). Defaults describe an
# unverified bare `docker build`; real builds must pass values from
# provenance-check/CI.
ARG DIRTY=false
ARG SOURCES=[]
ARG GO_LOGUTILS_REF=0e7b095dc2364b1006f3fd65616a1bb8b8cedd93

LABEL name="NGX Storage Quartet Operator" \
      maintainer="NGX Storage" \
      vendor="NGX Storage" \
      version="${VERSION}" \
      release="${RELEASE}" \
      summary="NGX Storage Quartet CSI operator" \
      description="Cluster-scoped singleton operator reconciling the nfs/fc/iscsi CSI driver profiles" \
      org.opencontainers.image.title="NGX Storage Quartet Operator" \
      org.opencontainers.image.vendor="NGX Storage" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${VCS_REF}" \
      org.opencontainers.image.source="https://gitlab.ngxio.com/ngx-storage/integrations/container-storage/operator/quartet" \
      org.opencontainers.image.licenses="Apache-2.0" \
      vcs-ref="${VCS_REF}" \
      vcs-type="git" \
      vcs-ref-dep-go-logutils="${GO_LOGUTILS_REF}" \
      io.ngxstorage.source.dirty="${DIRTY}" \
      io.ngxstorage.source.modules="${SOURCES}"

# The manager is a pure control-plane process: no host paths, no privileged
# access. Non-root 65532 matches config/manager/manager.yaml.
COPY --from=build /out/manager /manager

# Distribute the license with the image (Apache-2.0).
COPY LICENSE /LICENSE

RUN chmod 0755 /manager

USER 65532

ENTRYPOINT ["/manager"]
