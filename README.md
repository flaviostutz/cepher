# rbd-docker-plugin-container
Ceph RBD volume driver running in a container. Based on https://github.com/yp-engineering/rbd-docker-plugin

## Usage

* Run a sample Ceph Storage Cluster along with ceph-driver

docker-compose.yml

```
  ceph-driver:
    image: flaviostutz/ceph-docker-volume-driver
    environment:
      - CEPH_MONITOR_HOST=mon0
      - DEFAULT_POOL_NAME=mypool
      - DEFAULT_POOL_CREATE=true
      - DEFAULT_IMAGE_SIZE=1
      - CAN_AUTO_CREATE_VOLUMES=false
      - LOG_DEBUG=1
      - ETCD_URL=http://etcd0:2379
    volumes:
      - /run/docker/plugins:/run/docker/plugins
      - /mnt:/mnt
      - /dev:/dev
      - /sys:/sys

  etcd0:
    image: quay.io/coreos/etcd
    environment:
      - ETCD_LISTEN_CLIENT_URLS=http://0.0.0.0:2379
      - ETCD_ADVERTISE_CLIENT_URLS=http://etcd0:2379

  mon0:
    image: flaviostutz/ceph-monitor
    ports:
      - 6789:6789
    environment:
      - CREATE_CLUSTER=true
      - ETCD_URL=http://etcd0:2379

  mgr1:
    image: flaviostutz/ceph-manager
    ports:
      - 18443:8443 #dashboard https
      - 18003:8003 #restful https
      - 19283:9283 #prometheus
    environment:
      - LOG_LEVEL=0
      - PEER_MONITOR_HOST=mon0
      - ETCD_URL=http://etcd0:2379

  osd1:
    image: flaviostutz/ceph-osd
    environment:
      - PEER_MONITOR_HOST=mon0
      - OSD_EXT4_SUPPORT=true
      - OSD_JOURNAL_SIZE=512
      - ETCD_URL=http://etcd0:2379

  osd2:
    image: flaviostutz/ceph-osd
    environment:
      - PEER_MONITOR_HOST=mon0
      - OSD_EXT4_SUPPORT=true
      - OSD_JOURNAL_SIZE=512
      - ETCD_URL=http://etcd0:2379

```

* Prepare Ceph Pools in which you will store volume images

```
  # connect to a container that can be used as a Ceph Client
  docker-compose exec mgr1 bash

  # create a pool for volume images (each volume in Docker will be an image in RDB)
  osd pool mypool pg num = 128

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
docker run -it --rm --volume-driver=rbd --name second --volume mypool/myimage:/mnt/foo ubuntu /bin/bash -c "cat /mnt/foo/hello"

# on Ceph client machine, list the created image
docker-compose exec mgr1 bash
rbd ls default
```

## TODO
* Enhance error messages returned to Docker daemon (Plugin) when image cannot be created
