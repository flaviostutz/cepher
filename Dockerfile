FROM golang:1.10 AS BUILD

RUN go get -v github.com/Soulou/curl-unix-socket

#doing dependency build separated from source build optimizes time for developer, but is not required
#install external dependencies first
# ADD go-plugins-helpers/Gopkg.toml $GOPATH/src/go-plugins-helpers/
ADD /main.go $GOPATH/src/cepher/main.go
RUN go get -v cepher

#now build source code
ADD cepher $GOPATH/src/
RUN go get -v cepher


FROM flaviostutz/ceph-client:13.2.0.2

RUN apt-get update
RUN apt-get install -y librados-dev librbd-dev rbd-nbd

#default ENV values ignored when using managed plugins
ENV MONITOR_HOSTS ''
ENV CEPH_KEYRING_BASE64 ''
ENV ETCD_URL ''

ENV CEPH_AUTH 'cephx'
ENV CEPH_USER 'admin'
ENV CEPH_CLUSTER_NAME 'ceph'
ENV ENABLE_AUTO_CREATE_VOLUMES 'false'
ENV DEFAULT_IMAGE_SIZE 100
ENV DEFAULT_IMAGE_FS 'xfs'
ENV DEFAULT_IMAGE_FEATURES 'layering,striping,exclusive-lock,object-map,fast-diff,journaling'
ENV VOLUME_REMOVE_ACTION 'rename'
ENV DEFAULT_POOL_NAME 'volumes'
ENV DEFAULT_POOL_CREATE 'true'
ENV DEFAULT_POOL_PG_NUM 100
ENV DEFAULT_POOL_QUOTA_MAX_BYTES ''
ENV USE_RBD_KERNEL_MODULE false
ENV ENABLE_WRITE_LOCK true
ENV LOG_LEVEL 'info'

COPY --from=BUILD /go/bin/* /bin/
ADD startup.sh /
ADD ceph.conf.template /

# VOLUME [ "/run/docker/plugins" ]
# VOLUME [ "/dev" ]
# VOLUME [ "/sys" ]
# VOLUME [ "/lib" ]
# VOLUME [ "/mnt" ]
# VOLUME [ "/proc" ]

CMD [ "/startup.sh" ]
