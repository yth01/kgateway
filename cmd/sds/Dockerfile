ARG BASE_IMAGE

FROM $BASE_IMAGE

RUN apk -U upgrade

ARG TARGETARCH
COPY *sds-linux-$TARGETARCH /usr/local/bin/sds

USER 10101

ENTRYPOINT ["/usr/local/bin/sds"]
