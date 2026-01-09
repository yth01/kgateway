ARG BASE_IMAGE

FROM $BASE_IMAGE

RUN apk -U upgrade

ARG TARGETARCH
COPY *dummy-idp-linux-$TARGETARCH /usr/local/bin/dummy-idp

USER 10101

ENTRYPOINT ["/usr/local/bin/dummy-idp"]
