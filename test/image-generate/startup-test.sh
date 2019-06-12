#!/bin/bash
set -e
set -x

export PLUGIN_NAME="cepher"
export MOUNT_PATH="/mnt/cepher"

echo "ENV defaults don't come from Dockerfile when using managed plugins. Doing by hand..."
if [ "$MONITOR_HOSTS" == "" ]; then
    export MONITOR_HOSTS="172.20.10.10:6789"
fi 
if [ "$ETCD_URL" == "" ]; then
    export ETCD_URL="http://172.20.10.10:12379"
fi 
if [ "$CEPH_AUTH" == "" ]; then
    export CEPH_AUTH="cephx"
fi 
if [ "$CEPH_USER" == "" ]; then
    export CEPH_USER="admin"
fi 
if [ "$CEPH_CLUSTER_NAME" == "" ]; then
    export CEPH_CLUSTER_NAME="ceph"
fi 
if [ "$ENABLE_AUTO_CREATE_VOLUMES" == "" ]; then
    export ENABLE_AUTO_CREATE_VOLUMES="false"
fi 
if [ "$DEFAULT_IMAGE_SIZE" == "" ]; then
    export DEFAULT_IMAGE_SIZE="100"
fi 
if [ "$DEFAULT_IMAGE_FS" == "" ]; then
    export DEFAULT_IMAGE_FS="xfs"
fi 
if [ "$VOLUME_REMOVE_ACTION" == "" ]; then
    export VOLUME_REMOVE_ACTION="rename"
fi 
if [ "$DEFAULT_IMAGE_FEATURES" == "" ]; then
    export DEFAULT_IMAGE_FEATURES="layering"
fi 
if [ "$DEFAULT_POOL_NAME" == "" ]; then
    export DEFAULT_POOL_NAME="volumes"
fi 
if [ "$DEFAULT_POOL_CREATE" == "" ]; then
    export DEFAULT_POOL_CREATE="true"
fi 
if [ "$DEFAULT_POOL_PG_NUM" == "" ]; then
    export DEFAULT_POOL_PG_NUM="100"
fi 
if [ "$USE_RBD_KERNEL_MODULE" == "" ]; then
    export USE_RBD_KERNEL_MODULE="false"
fi 
if [ "$ENABLE_WRITE_LOCK" == "" ]; then
    export ENABLE_WRITE_LOCK="true"
fi 
if [ "$LOG_LEVEL" == "" ]; then
    export LOG_LEVEL="info"
fi 

echo "Starting CEPHER with MONITOR_HOSTS=$MONITOR_HOSTS \
    ETCD_URL=$ETCD_URL \
    CEPH_KEYRING_BASE64=$CEPH_KEYRING_BASE64 \
    CEPH_AUTH=$CEPH_AUTH \
    CEPH_USER=$CEPH_USER \
    CEPH_CLUSTER_NAME=$CEPH_CLUSTER_NAME \
    DEFAULT_POOL_NAME=$DEFAULT_POOL_NAME \
    MOUNT_PATH=$MOUNT_PATH \
    ENABLE_AUTO_CREATE_VOLUMES=$ENABLE_AUTO_CREATE_VOLUMES \
    DEFAULT_IMAGE_FS=$DEFAULT_IMAGE_FS \
    DEFAULT_IMAGE_SIZE=$DEFAULT_IMAGE_SIZE \
    ENABLE_WRITE_LOCK=$ENABLE_WRITE_LOCK \
    LOG_LEVEL=$LOG_LEVEL"

if [ ! -f /etc/ceph/ceph.conf ]; then
    echo "/etc/ceph/ceph.conf not found. creating it..."
    /initialize.sh
fi

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

go test -v driver_test.go driver.go utils.go