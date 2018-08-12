# rbd-docker-plugin-container
Ceph RBD volume driver running in a container. Based on https://github.com/yp-engineering/rbd-docker-plugin

## Usage

* Set HOST_IP on .env to your machine IP
* Run a sample Ceph Storage Cluster along with ceph-driver

docker-compose.yml

```
  ceph-driver:
    image: flaviostutz/ceph-docker-volume-driver
    network_mode: host
    environment:
      - MONITOR_HOSTS=${HOST_IP}:16789,${HOST_IP}:26789,${HOST_IP}:16789
      - DEFAULT_IMAGE_SIZE=1
      - ENABLE_AUTO_CREATE_VOLUMES=false
      - LOG_DEBUG=1
      - ETCD_URL=http://${HOST_IP}:12379
    privileged: true
    volumes:
      - /run/docker/plugins:/run/docker/plugins
      - /mnt:/mnt
      - /dev:/dev
      - /sys:/sys
      - /lib:/lib

  etcd0:
    image: quay.io/coreos/etcd
    network_mode: host
    environment:
      - ETCD_LISTEN_CLIENT_URLS=http://0.0.0.0:12379
      - ETCD_ADVERTISE_CLIENT_URLS=http://${HOST_IP}:12379

  mon1:
    image: flaviostutz/ceph-monitor
    network_mode: host
    environment:
      - LOG_LEVEL=0
      - CREATE_CLUSTER=true
      - ETCD_URL=http://${HOST_IP}:12379
      - PEER_MONITOR_HOSTS=${HOST_IP}:26789,${HOST_IP}:36789
      - MONITOR_ADVERTISE_ADDRESS=${HOST_IP}:16789
      - MONITOR_BIND_PORT=16789

  mon2:
    image: flaviostutz/ceph-monitor
    network_mode: host
    environment:
      - ETCD_URL=http://${HOST_IP}:12379
      - PEER_MONITOR_HOSTS=${HOST_IP}:16789,${HOST_IP}:36789
      - MONITOR_ADVERTISE_ADDRESS=${HOST_IP}:26789
      - MONITOR_BIND_PORT=26789

  mon3:
    image: flaviostutz/ceph-monitor
    network_mode: host
    environment:
      - ETCD_URL=http://${HOST_IP}:12379
      - PEER_MONITOR_HOSTS=${HOST_IP}:16789,${HOST_IP}:26789
      - MONITOR_ADVERTISE_ADDRESS=${HOST_IP}:36789
      - MONITOR_BIND_PORT=36789

  mgr1:
    image: flaviostutz/ceph-manager
    ports:
      - 18443:8443 #dashboard https
      - 18003:8003 #restful https
      - 19283:9283 #prometheus
    environment:
      - MONITOR_HOSTS=${HOST_IP}:16789,${HOST_IP}:26789,${HOST_IP}:16789
      - ETCD_URL=http://${HOST_IP}:12379

  osd1:
    image: flaviostutz/ceph-osd
    network_mode: host
    pid: host
    environment:
      - LOG_LEVEL=0
      - MONITOR_HOSTS=${HOST_IP}:16789,${HOST_IP}:26789,${HOST_IP}:36789
      - OSD_EXT4_SUPPORT=true
      - OSD_JOURNAL_SIZE=512
      - OSD_CRUSH_LOCATION=root=default host=host1
      - ETCD_URL=http://${HOST_IP}:12379
      # - OSD_PUBLIC_IP=${HOST_IP}
      # - OSD_CLUSTER_IP=${HOST_IP}

  osd2:
    image: flaviostutz/ceph-osd
    network_mode: host
    pid: host
    environment:
      - LOG_LEVEL=0
      - MONITOR_HOSTS=${HOST_IP}:16789,${HOST_IP}:26789,${HOST_IP}:36789
      - OSD_EXT4_SUPPORT=true
      - OSD_JOURNAL_SIZE=512
      - OSD_CRUSH_LOCATION=root=default host=host2
      - ETCD_URL=http://${HOST_IP}:12379

  osd3:
    image: flaviostutz/ceph-osd
    network_mode: host
    pid: host
    environment:
      - LOG_LEVEL=0
      - MONITOR_HOSTS=${HOST_IP}:16789,${HOST_IP}:26789,${HOST_IP}:36789
      - OSD_EXT4_SUPPORT=true
      - OSD_JOURNAL_SIZE=512
      - OSD_CRUSH_LOCATION=root=default host=host3
      - ETCD_URL=http://${HOST_IP}:12379

```

* There is a default "docker-volumes" pool that this image prepares for you, but if you with to create a customized pool, follow these steps

```
  # connect to a container that can be used as a Ceph Client
  docker-compose exec mgr1 bash

  # create a pool for volume images (each volume in Docker will be an image in RDB)
  ceph osd pool create mypool 30
  rbd pool init mypool

  # set max bytes for this pool (optional)
  ceph osd pool set-quota mypool data max_bytes 1000000000

  # initialize this pool with rbd
  rbd pool init mypool
```

  See more at http://docs.ceph.com/docs/master/start/quick-rbd/ and http://docs.ceph.com/docs/jewel/rados/operations/pools/

* Run your container with a persisted volume in Ceph

```
# run a container that will make the first usage of the image, so it will be
# created on Ceph during volume creation
# after echoing to a file in the newly created volume, this container will exit 
# and remove its instance's data, but not the image data.
docker run -it --rm --volume-driver=rbd --name first --volume mypool/myimage:/mnt/foo ubuntu /bin/bash -c "echo -n 'Hello ' >> /mnt/foo/hello"

# run a second docker container, but use the same volume/image from the previous
# step so that we can see the persisted data
docker run -it --rm --volume-driver=rbd --name second --volume volumes/myimage:/mnt/foo ubuntu /bin/bash -c "cat /mnt/foo/hello"

# on Ceph client machine, list the created image
docker-compose exec mgr1 bash
rbd ls default
```

## TODO
* Enhance error messages returned to Docker daemon (Plugin) when image cannot be created
