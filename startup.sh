#!/bin/bash
set -e
set -x

export MOUNT_PATH="/mnt/rbd-driver/$PLUGIN_NAME"
export RBD_DOCKER_PLUGIN_DEBUG=$LOG_DEBUG

mkdir -p $MOUNT_PATH

echo "Preparing default Ceph pool $DEFAULT_POOL_NAME..."
set +e
R=$(ceph osd pool ls | grep ${DEFAULT_POOL_NAME} -x)
set -e
if [ "$R" != "" ]; then
    echo "Pool was found in Ceph cluster"
else
    echo "Pool was not found in Ceph cluster"
    if [ "$DEFAULT_POOL_CREATE" == "true" ]; then
        echo "Creating pool ${DEFAULT_POOL_NAME}..."
        ceph osd pool create ${DEFAULT_POOL_NAME} ${DEFAULT_POOL_PG_NUM}
        if [ "$DEFAULT_POOL_QUOTA_MAX_BYTES" != "" ]; then
            echo "Setting quota max bytes to ${DEFAULT_POOL_QUOTA_MAX_BYTES}..."
            ceph osd pool set-quota ${DEFAULT_POOL_NAME} max_bytes ${DEFAULT_POOL_QUOTA_MAX_BYTES}
        fi
    fi
fi
rbd pool init ${DEFAULT_POOL_NAME}

echo "Starting rbd-docker-plugin..."
rbd-docker-plugin \
    --name $PLUGIN_NAME \
    --user $CEPH_USER \
    --cluster $CEPH_CLUSTER_NAME \
    --pool $DEFAULT_POOL_NAME \
    --mount $MOUNT_PATH \
    --create $CAN_AUTO_CREATE_VOLUMES \
    --fs $DEFAULT_IMAGE_FS \
    --size $DEFAULT_IMAGE_SIZE \
    --config /etc/ceph/ceph.conf

