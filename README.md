# cepher
Docker Volume Plugin that enables the management of volumes on Ceph RBD backends.
This is a hard fork of https://github.com/yp-engineering/rbd-docker-plugin

This plugin will perform the following:

  - The name used on the volume will be used for locating the Ceph image (ex.: mypool/myvolume)
  - When creating/removing a volume, it will try to locate an image with that name and perform operations on Ceph cluster
  - When mounting a volume to a container, it will try to locate that image, create it if doesn't exist yet, map it to the host, format it using a specified filesystem (xfs is default), mount the device to an directory and Docker will bind that directory to the container
  - Only one mapping is permitted per image, so we will perform an exclusive lock on Ceph images to avoid corruption.

#### Performance note

Modern Linux Kernel comes with a Ceph module for mapping images as virtual devices on OS. This module is very efficient thou it doesn't support recent features on Ceph Images, like journaling and fast-diff. By default this plugin will use the 'rbd-nbd' instead of the kernel module. It does the same mapping as the kernel module, but with a little less performance but supports all Ceph Image features. If you with to force Kernel Module usage, set USE_RBD_KERNEL_MODULE to false.

## Usage (managed plugin)

* Set HOST_IP on .env to your machine IP
* Run a sample Ceph Storage Cluster

docker-compose.yml

```
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
      - MONITOR_ADVERTISE_ADDRESS=${HOST_IP}:16789
      - MONITOR_BIND_PORT=16789

  mgr1:
    image: flaviostutz/ceph-manager
    ports:
      - 18443:8443 #dashboard https
      - 18003:8003 #restful https
      - 19283:9283 #prometheus
    environment:
      - MONITOR_HOSTS=${HOST_IP}:16789
      - ETCD_URL=http://${HOST_IP}:12379

  osd1:
    image: flaviostutz/ceph-osd
    network_mode: host
    pid: host
    environment:
      - LOG_LEVEL=0
      - MONITOR_HOSTS=${HOST_IP}:16789
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
      - MONITOR_HOSTS=${HOST_IP}:16789
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
      - MONITOR_HOSTS=${HOST_IP}:16789
      - OSD_EXT4_SUPPORT=true
      - OSD_JOURNAL_SIZE=512
      - OSD_CRUSH_LOCATION=root=default host=host3
      - ETCD_URL=http://${HOST_IP}:12379

```

* Run Cepher plugin

```
docker plugin install flaviostutz/cepher \
  --grant-all-permissions \
  --alias=cepher \
  MONITOR_HOSTS="${HOST_IP}:16789,${HOST_IP}:26789,${HOST_IP}:16789" \
  ETCD_URL="http://${HOST_IP}:12379" \
  DEFAULT_IMAGE_SIZE=1 \
  ENABLE_AUTO_CREATE_VOLUMES=true

```

* Test it!

```
docker run -it --rm --volume-driver=cepher --name first --volume volumes/myimage:/mnt/foo ubuntu /bin/bash -c "echo -n 'Hello ' >> /mnt/foo/hello"

docker run -it --rm --volume-driver=cepher --name second --volume volumes/myimage:/mnt/foo ubuntu /bin/bash -c "cat /mnt/foo/hello"

```


## Usage (non managed plugin run)

* Set HOST_IP on .env to your machine IP
* Run a sample Ceph Storage Cluster along with cepher

docker-compose.yml

```
  cepher:
    image: flaviostutz/cepher
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

* There is a default "volumes" pool that this image prepares for you, but if you wish to create a customized pool, follow these steps

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
docker run -it --rm --volume-driver=cepher --name first --volume volumes/myimage:/mnt/foo ubuntu /bin/bash -c "echo -n 'Hello ' >> /mnt/foo/hello"

# run a second docker container, but use the same volume/image from the previous
# step so that we can see the persisted data
docker run -it --rm --volume-driver=cepher --name second --volume volumes/myimage:/mnt/foo ubuntu /bin/bash -c "cat /mnt/foo/hello"

# on Ceph client machine, list the created image
docker-compose exec mgr1 bash
rbd ls default
```

## ENV configurations

* MONITOR\_HOSTS - comma separated list of [monitor-ip:port]
* CEPH\_KEYRING\_BASE64 - base64 encoded keyring to be used to connect to Ceph Cluster
* ETCD\_URL - if defined, the plugin will search for a base64 encoded keyring at /[cluster-name]/keyring
* CEPH\_AUTH - 'none' or 'cephx'
* CEPH\_USER - user name to use to connect to Ceph
* CEPH\_CLUSTER\_NAME - Ceph cluster name
* ENABLE\_AUTO\_CREATE\_VOLUMES - whatever this plugin will create new images on Ceph cluster if the corresponding image is not found
* DEFAULT\_IMAGE\_SIZE - default image size for newly created images. maybe overridden by opt
* DEFAULT\_IMAGE\_FS - default image filesystem for newly created images. maybe overridden by opt
* DEFAULT\_IMAGE\_FEATURES - default image features for newly created images. maybe overridden by opt
* VOLUME\_REMOVE\_ACTION - 'ignore': does nothing on Ceph Cluster when a volume is deleted; 'delete': deletes the corresponding image from Ceph Cluster (irreversible!); 'rename' - renames the corresponding Ceph Image to zz_[image name]
* DEFAULT\_POOL\_NAME - default pool name when not specified in volume name
* DEFAULT\_POOL\_CREATE - whatever during plugin initialization, it will look for the default pool and create it or not
* DEFAULT\_POOL\_PG_NUM - number of PGs for the default pool when creating it
* DEFAULT\_POOL\_QUOTA_MAX_BYTES - max bytes size for the default pool during creation
* USE_RBD\_KERNEL\_MODULE - if true, will use the Linux RBD Kernel Module that has greater performance, but doesn't support recent image features. if false, will use official Ceph rbd-nbd tool for mapping the images that supports all recent image features. false is default
* LOG\_LEVEL - debug, info, warning or error

## Driver opt configurations

* pool - name of Ceph pool
* name - name of Ceph image
* size - image size when creating a new image in MB
* fstype - filesystem type to create on newly created images. mkfs.[fstype] must be present in OS
* features - Ceph image features applied to newly created images. defaults to 'layering,striping,exclusive-lock,object-map,fast-diff,journaling'