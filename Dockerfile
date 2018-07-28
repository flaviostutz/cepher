FROM golang:1.10 AS BUILD
RUN go get github.com/yp-engineering/rbd-docker-plugin
RUN go get github.com/Soulou/curl-unix-socket

FROM flaviostutz/ceph-base
COPY --from=BUILD /go/bin/* /bin/

RUN apt-get update
RUN apt-get install -y librados-dev librbd-dev

ENV CEPH_MONITOR_HOST ''
ENV CEPH_KEYRING_BASE64 ''

ENV CEPH_AUTH 'cephx'
ENV CEPH_USER 'admin'
ENV CEPH_CLUSTER_NAME 'ceph'
ENV PLUGIN_NAME 'rbd'
ENV CAN_AUTO_CREATE_VOLUMES 'false'
ENV DEFAULT_IMAGE_SIZE 5
ENV DEFAULT_IMAGE_FS 'xfs'
ENV DEFAULT_POOL_NAME 'docker-volumes'
ENV DEFAULT_POOL_CREATE 'true'
ENV DEFAULT_POOL_PG_NUM 100
ENV DEFAULT_POOL_QUOTA_MAX_BYTES ''
ENV LOG_DEBUG 0

ADD startup.sh /
ADD ceph.conf.template /

VOLUME [ "/run/docker/plugins" ]
VOLUME [ "/dev" ]
VOLUME [ "/sys" ]
VOLUME [ "/mnt" ]

CMD [ "/startup.sh" ]
