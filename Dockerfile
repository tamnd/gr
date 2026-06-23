# Consumed by GoReleaser: it copies the already cross-compiled binary out of
# the build context rather than compiling, so the image build is fast and uses
# the same static binary every other artifact ships.
#
# gr is pure Go with no runtime dependencies beyond CA certificates, so the
# image is minimal: the static binary on Alpine.
#
# GoReleaser builds one multi-platform image with buildx and stages each
# platform's binary under a $TARGETPLATFORM directory in the build context,
# so the COPY line selects the right one through the automatic TARGETPLATFORM
# build arg.
FROM alpine:3.21

ARG TARGETPLATFORM

RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -H -u 10001 gr

COPY $TARGETPLATFORM/gr /usr/bin/gr

USER gr
WORKDIR /data

ENTRYPOINT ["/usr/bin/gr"]
