FROM golang:1.12.4-stretch AS BUILD

RUN apt-get update
RUN apt-get install -y librados-dev librbd-dev rbd-nbd

RUN go get -v github.com/Soulou/curl-unix-socket

RUN mkdir /cepher
WORKDIR /cepher

ADD go.mod .
ADD go.sum .
RUN go mod download

#now build source code
ADD cepher/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -ldflags '-extldflags "-static"' -o /go/bin/cepher .

### ==> Mount New Image...

FROM flaviostutz/ceph-client:13.2.5

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
ENV LOG_LEVEL 'info'

COPY --from=BUILD /go/bin/* /bin/
ADD startup.sh /
ADD ceph.conf.template /

CMD [ "/startup.sh" ]
