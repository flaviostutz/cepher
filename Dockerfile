FROM golang:1.10 AS BUILD

#doing dependency build separated from source build optimizes time for developer, but is not required
#install external dependencies first
# ADD rbd-docker-plugin/Gopkg.toml $GOPATH/src/rbd-docker-plugin/
ADD /main.go $GOPATH/src/rbd-docker-plugin/main.go
RUN go get -v rbd-docker-plugin
RUN go get -v github.com/Soulou/curl-unix-socket

#now build source code
ADD src $GOPATH/src/rbd-docker-plugin
RUN go get -v rbd-docker-plugin


FROM flaviostutz/ceph-client

RUN apt-get update
RUN apt-get install -y librados-dev librbd-dev

ENV MONITOR_HOSTS ''
ENV CEPH_KEYRING_BASE64 ''
ENV ETCD_URL ''

ENV CEPH_AUTH 'cephx'
ENV CEPH_USER 'admin'
ENV CEPH_CLUSTER_NAME 'ceph'
ENV PLUGIN_NAME 'rbd'
ENV ENABLE_AUTO_CREATE_VOLUMES 'false'
ENV DEFAULT_IMAGE_SIZE 5
ENV DEFAULT_IMAGE_FS 'xfs'
ENV DEFAULT_POOL_NAME 'volumes'
ENV DEFAULT_POOL_CREATE 'true'
ENV DEFAULT_POOL_PG_NUM 100
ENV DEFAULT_POOL_QUOTA_MAX_BYTES ''
ENV LOG_DEBUG 0

COPY --from=BUILD /go/bin/* /bin/
ADD startup.sh /
ADD ceph.conf.template /

# VOLUME [ "/run/docker/plugins" ]
# VOLUME [ "/dev" ]
# VOLUME [ "/sys" ]
# VOLUME [ "/lib" ]
# VOLUME [ "/mnt" ]

CMD [ "/startup.sh" ]
